// Copyright (c) 2012 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gocql

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/gocql/gocql/internal/lru"
	"io"
	"io/ioutil"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gocql/gocql/internal/streams"
)

var (
	approvedAuthenticators = [...]string{
		"org.apache.cassandra.auth.PasswordAuthenticator",
		"com.instaclustr.cassandra.auth.SharedSecretAuthenticator",
	}
)

func approve(authenticator string) bool {
	for _, s := range approvedAuthenticators {
		if authenticator == s {
			return true
		}
	}
	return false
}

//JoinHostPort is a utility to return a address string that can be used
//gocql.Conn to form a connection with a host.
func JoinHostPort(addr string, port int) string {
	addr = strings.TrimSpace(addr)
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, strconv.Itoa(port))
	}
	return addr
}

type Authenticator interface {
	Challenge(req []byte) (resp []byte, auth Authenticator, err error)
	Success(data []byte) error
}

type PasswordAuthenticator struct {
	Username string
	Password string
}

func (p PasswordAuthenticator) Challenge(req []byte) ([]byte, Authenticator, error) {
	if !approve(string(req)) {
		return nil, nil, fmt.Errorf("unexpected authenticator %q", req)
	}
	resp := make([]byte, 2+len(p.Username)+len(p.Password))
	resp[0] = 0
	copy(resp[1:], p.Username)
	resp[len(p.Username)+1] = 0
	copy(resp[2+len(p.Username):], p.Password)
	return resp, nil, nil
}

func (p PasswordAuthenticator) Success(data []byte) error {
	return nil
}

type SslOptions struct {
	tls.Config

	// CertPath and KeyPath are optional depending on server
	// config, but both fields must be omitted to avoid using a
	// client certificate
	CertPath string
	KeyPath  string
	CaPath   string //optional depending on server config
	// If you want to verify the hostname and server cert (like a wildcard for cass cluster) then you should turn this on
	// This option is basically the inverse of InSecureSkipVerify
	// See InSecureSkipVerify in http://golang.org/pkg/crypto/tls/ for more info
	EnableHostVerification bool
}

type ConnConfig struct {
	ProtoVersion  int
	CQLVersion    string
	Timeout       time.Duration
	Compressor    Compressor
	Authenticator Authenticator
	Keepalive     time.Duration
	tlsConfig     *tls.Config
}

type ConnErrorHandler interface {
	HandleError(conn *Conn, err error, closed bool)
}

type connErrorHandlerFn func(conn *Conn, err error, closed bool)

func (fn connErrorHandlerFn) HandleError(conn *Conn, err error, closed bool) {
	fn(conn, err, closed)
}

// How many timeouts we will allow to occur before the connection is closed
// and restarted. This is to prevent a single query timeout from killing a connection
// which may be serving more queries just fine.
// Default is 10, should not be changed concurrently with queries.
var TimeoutLimit int64 = 10

// Conn is a single connection to a Cassandra node. It can be used to execute
// queries, but users are usually advised to use a more reliable, higher
// level API.
type Conn struct {
	conn    net.Conn
	r       *bufio.Reader
	timeout time.Duration
	cfg     *ConnConfig

	headerBuf []byte

	streams *streams.IDGenerator
	mu      sync.RWMutex
	calls   map[int]*callReq

	errorHandler    ConnErrorHandler
	compressor      Compressor
	auth            Authenticator
	addr            string
	version         uint8
	currentKeyspace string
	started         bool

	session *Session

	closed int32
	quit   chan struct{}

	timeouts int64
}

// Connect establishes a connection to a Cassandra node.
func Connect(addr string, cfg *ConnConfig, errorHandler ConnErrorHandler, session *Session) (*Conn, error) {
	var (
		err  error
		conn net.Conn
	)

	dialer := &net.Dialer{
		Timeout: cfg.Timeout,
	}

	if cfg.tlsConfig != nil {
		// the TLS config is safe to be reused by connections but it must not
		// be modified after being used.
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, cfg.tlsConfig)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}

	if err != nil {
		return nil, err
	}

	// going to default to proto 2
	if cfg.ProtoVersion < protoVersion1 || cfg.ProtoVersion > protoVersion4 {
		log.Printf("unsupported protocol version: %d using 2\n", cfg.ProtoVersion)
		cfg.ProtoVersion = 2
	}

	headerSize := 8
	if cfg.ProtoVersion > protoVersion2 {
		headerSize = 9
	}

	c := &Conn{
		conn:         conn,
		r:            bufio.NewReader(conn),
		cfg:          cfg,
		calls:        make(map[int]*callReq),
		timeout:      cfg.Timeout,
		version:      uint8(cfg.ProtoVersion),
		addr:         conn.RemoteAddr().String(),
		errorHandler: errorHandler,
		compressor:   cfg.Compressor,
		auth:         cfg.Authenticator,
		headerBuf:    make([]byte, headerSize),
		quit:         make(chan struct{}),
		session:      session,
		streams:      streams.New(cfg.ProtoVersion),
	}

	if cfg.Keepalive > 0 {
		c.setKeepalive(cfg.Keepalive)
	}

	go c.serve()

	if err := c.startup(); err != nil {
		conn.Close()
		return nil, err
	}
	c.started = true

	return c, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	}

	return c.conn.Write(p)
}

func (c *Conn) Read(p []byte) (n int, err error) {
	const maxAttempts = 5

	for i := 0; i < maxAttempts; i++ {
		var nn int
		if c.timeout > 0 {
			c.conn.SetReadDeadline(time.Now().Add(c.timeout))
		}

		nn, err = io.ReadFull(c.r, p[n:])
		n += nn
		if err == nil {
			break
		}

		if verr, ok := err.(net.Error); !ok || !verr.Temporary() {
			break
		}
	}

	return
}

func (c *Conn) startup() error {
	m := map[string]string{
		"CQL_VERSION": c.cfg.CQLVersion,
	}

	if c.compressor != nil {
		m["COMPRESSION"] = c.compressor.Name()
	}

	framer, err := c.exec(&writeStartupFrame{opts: m}, nil)
	if err != nil {
		return err
	}

	frame, err := framer.parseFrame()
	if err != nil {
		return err
	}

	switch v := frame.(type) {
	case error:
		return v
	case *readyFrame:
		return nil
	case *authenticateFrame:
		return c.authenticateHandshake(v)
	default:
		return NewErrProtocol("Unknown type of response to startup frame: %s", v)
	}
}

func (c *Conn) authenticateHandshake(authFrame *authenticateFrame) error {
	if c.auth == nil {
		return fmt.Errorf("authentication required (using %q)", authFrame.class)
	}

	resp, challenger, err := c.auth.Challenge([]byte(authFrame.class))
	if err != nil {
		return err
	}

	req := &writeAuthResponseFrame{data: resp}

	for {
		framer, err := c.exec(req, nil)
		if err != nil {
			return err
		}

		frame, err := framer.parseFrame()
		if err != nil {
			return err
		}

		switch v := frame.(type) {
		case error:
			return v
		case *authSuccessFrame:
			if challenger != nil {
				return challenger.Success(v.data)
			}
			return nil
		case *authChallengeFrame:
			resp, challenger, err = challenger.Challenge(v.data)
			if err != nil {
				return err
			}

			req = &writeAuthResponseFrame{
				data: resp,
			}
		default:
			return fmt.Errorf("unknown frame response during authentication: %v", v)
		}

		framerPool.Put(framer)
	}
}

func (c *Conn) closeWithError(err error) {
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return
	}

	if err != nil {
		// we should attempt to deliver the error back to the caller if it
		// exists
		c.mu.RLock()
		for _, req := range c.calls {
			// we need to send the error to all waiting queries, put the state
			// of this conn into not active so that it can not execute any queries.
			if err != nil {
				select {
				case req.resp <- err:
				default:
				}
			}
		}
		c.mu.RUnlock()
	}

	// if error was nil then unblock the quit channel
	close(c.quit)
	c.conn.Close()

	if c.started && err != nil {
		c.errorHandler.HandleError(c, err, true)
	}
}

func (c *Conn) Close() {
	c.closeWithError(nil)
}

// Serve starts the stream multiplexer for this connection, which is required
// to execute any queries. This method runs as long as the connection is
// open and is therefore usually called in a separate goroutine.
func (c *Conn) serve() {
	var (
		err error
	)

	for {
		err = c.recv()
		if err != nil {
			break
		}
	}

	c.closeWithError(err)
}

func (c *Conn) discardFrame(head frameHeader) error {
	_, err := io.CopyN(ioutil.Discard, c, int64(head.length))
	if err != nil {
		return err
	}
	return nil
}

func (c *Conn) recv() error {
	// not safe for concurrent reads

	// read a full header, ignore timeouts, as this is being ran in a loop
	// TODO: TCP level deadlines? or just query level deadlines?
	if c.timeout > 0 {
		c.conn.SetReadDeadline(time.Time{})
	}

	// were just reading headers over and over and copy bodies
	head, err := readHeader(c.r, c.headerBuf)
	if err != nil {
		return err
	}

	if head.stream > c.streams.NumStreams {
		return fmt.Errorf("gocql: frame header stream is beyond call exepected bounds: %d", head.stream)
	} else if head.stream == -1 {
		// TODO: handle cassandra event frames, we shouldnt get any currently
		framer := newFramer(c, c, c.compressor, c.version)
		if err := framer.readFrame(&head); err != nil {
			return err
		}
		go c.session.handleEvent(framer)
		return nil
	} else if head.stream <= 0 {
		// reserved stream that we dont use, probably due to a protocol error
		// or a bug in Cassandra, this should be an error, parse it and return.
		framer := newFramer(c, c, c.compressor, c.version)
		if err := framer.readFrame(&head); err != nil {
			return err
		}
		defer framerPool.Put(framer)

		frame, err := framer.parseFrame()
		if err != nil {
			return err
		}

		switch v := frame.(type) {
		case error:
			return fmt.Errorf("gocql: error on stream %d: %v", head.stream, v)
		default:
			return fmt.Errorf("gocql: received frame on stream %d: %v", head.stream, frame)
		}
	}

	c.mu.RLock()
	call, ok := c.calls[head.stream]
	c.mu.RUnlock()
	if call == nil || call.framer == nil || !ok {
		log.Printf("gocql: received response for stream which has no handler: header=%v\n", head)
		return c.discardFrame(head)
	}

	err = call.framer.readFrame(&head)
	if err != nil {
		// only net errors should cause the connection to be closed. Though
		// cassandra returning corrupt frames will be returned here as well.
		if _, ok := err.(net.Error); ok {
			return err
		}
	}

	// we either, return a response to the caller, the caller timedout, or the
	// connection has closed. Either way we should never block indefinatly here
	select {
	case call.resp <- err:
	case <-call.timeout:
		c.releaseStream(head.stream)
	case <-c.quit:
	}

	return nil
}

type callReq struct {
	// could use a waitgroup but this allows us to do timeouts on the read/send
	resp     chan error
	framer   *framer
	timeout  chan struct{} // indicates to recv() that a call has timedout
	streamID int           // current stream in use
}

func (c *Conn) releaseStream(stream int) {
	c.mu.Lock()
	call := c.calls[stream]
	if call != nil && stream != call.streamID {
		panic(fmt.Sprintf("attempt to release streamID with ivalid stream: %d -> %+v\n", stream, call))
	} else if call == nil {
		panic(fmt.Sprintf("releasing a stream not in use: %d", stream))
	}
	delete(c.calls, stream)
	c.mu.Unlock()

	streamPool.Put(call)
	c.streams.Clear(stream)
}

func (c *Conn) handleTimeout() {
	if atomic.AddInt64(&c.timeouts, 1) > TimeoutLimit {
		c.closeWithError(ErrTooManyTimeouts)
	}
}

var (
	streamPool = sync.Pool{
		New: func() interface{} {
			return &callReq{
				resp: make(chan error),
			}
		},
	}
)

func (c *Conn) exec(req frameWriter, tracer Tracer) (*framer, error) {
	// TODO: move tracer onto conn
	stream, ok := c.streams.GetStream()
	if !ok {
		fmt.Println(c.streams)
		return nil, ErrNoStreams
	}

	// resp is basically a waiting semaphore protecting the framer
	framer := newFramer(c, c, c.compressor, c.version)

	c.mu.Lock()
	call := c.calls[stream]
	if call != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("attempting to use stream already in use: %d -> %d", stream, call.streamID)
	} else {
		call = streamPool.Get().(*callReq)
	}
	c.calls[stream] = call
	c.mu.Unlock()

	call.framer = framer
	call.timeout = make(chan struct{})
	call.streamID = stream

	if tracer != nil {
		framer.trace()
	}

	err := req.writeFrame(framer, stream)
	if err != nil {
		// I think this is the correct thing to do, im not entirely sure. It is not
		// ideal as readers might still get some data, but they probably wont.
		// Here we need to be careful as the stream is not available and if all
		// writes just timeout or fail then the pool might use this connection to
		// send a frame on, with all the streams used up and not returned.
		c.closeWithError(err)
		return nil, err
	}

	var timeoutCh <-chan time.Time
	if c.timeout > 0 {
		timeoutCh = time.After(c.timeout)
	}

	select {
	case err := <-call.resp:
		if err != nil {
			if !c.Closed() {
				// if the connection is closed then we cant release the stream,
				// this is because the request is still outstanding and we have
				// been handed another error from another stream which caused the
				// connection to close.
				c.releaseStream(stream)
			}
			return nil, err
		}
	case <-timeoutCh:
		close(call.timeout)
		c.handleTimeout()
		return nil, ErrTimeoutNoResponse
	case <-c.quit:
		return nil, ErrConnectionClosed
	}

	// dont release the stream if detect a timeout as another request can reuse
	// that stream and get a response for the old request, which we have no
	// easy way of detecting.
	//
	// Ensure that the stream is not released if there are potentially outstanding
	// requests on the stream to prevent nil pointer dereferences in recv().
	defer c.releaseStream(stream)

	if v := framer.header.version.version(); v != c.version {
		return nil, NewErrProtocol("unexpected protocol version in response: got %d expected %d", v, c.version)
	}

	return framer, nil
}

type preparedStatment struct {
	id       []byte
	request  preparedMetadata
	response resultMetadata
}

type inflightPrepare struct {
	wg  sync.WaitGroup
	err error

	preparedStatment *preparedStatment
}

func (c *Conn) prepareStatement(stmt string, tracer Tracer) (*preparedStatment, error) {
	stmtCacheKey := c.session.stmtsLRU.keyFor(c.addr, c.currentKeyspace, stmt)
	flight, ok := c.session.stmtsLRU.execIfMissing(stmtCacheKey, func(lru *lru.Cache) *inflightPrepare {
		flight := new(inflightPrepare)
		flight.wg.Add(1)
		lru.Add(stmtCacheKey, flight)
		return flight
	})

	if ok {
		flight.wg.Wait()
		return flight.preparedStatment, flight.err
	}

	prep := &writePrepareFrame{
		statement: stmt,
	}

	framer, err := c.exec(prep, tracer)
	if err != nil {
		flight.err = err
		flight.wg.Done()
		return nil, err
	}

	frame, err := framer.parseFrame()
	if err != nil {
		flight.err = err
		flight.wg.Done()
		return nil, err
	}

	// TODO(zariel): tidy this up, simplify handling of frame parsing so its not duplicated
	// everytime we need to parse a frame.
	if len(framer.traceID) > 0 {
		tracer.Trace(framer.traceID)
	}

	switch x := frame.(type) {
	case *resultPreparedFrame:
		flight.preparedStatment = &preparedStatment{
			// defensivly copy as we will recycle the underlying buffer after we
			// return.
			id: copyBytes(x.preparedID),
			// the type info's should _not_ have a reference to the framers read buffer,
			// therefore we can just copy them directly.
			request:  x.reqMeta,
			response: x.respMeta,
		}
	case error:
		flight.err = x
	default:
		flight.err = NewErrProtocol("Unknown type in response to prepare frame: %s", x)
	}
	flight.wg.Done()

	if flight.err != nil {
		c.session.stmtsLRU.remove(stmtCacheKey)
	}

	framerPool.Put(framer)

	return flight.preparedStatment, flight.err
}

func (c *Conn) executeQuery(qry *Query) *Iter {
	params := queryParams{
		consistency: qry.cons,
	}

	// frame checks that it is not 0
	params.serialConsistency = qry.serialCons
	params.defaultTimestamp = qry.defaultTimestamp

	if len(qry.pageState) > 0 {
		params.pagingState = qry.pageState
	}
	if qry.pageSize > 0 {
		params.pageSize = qry.pageSize
	}

	var (
		frame frameWriter
		info  *preparedStatment
	)

	if qry.shouldPrepare() {
		// Prepare all DML queries. Other queries can not be prepared.
		var err error
		info, err = c.prepareStatement(qry.stmt, qry.trace)
		if err != nil {
			return &Iter{err: err}
		}

		var values []interface{}

		if qry.binding == nil {
			values = qry.values
		} else {
			values, err = qry.binding(&QueryInfo{
				Id:          info.id,
				Args:        info.request.columns,
				Rval:        info.response.columns,
				PKeyColumns: info.request.pkeyColumns,
			})

			if err != nil {
				return &Iter{err: err}
			}
		}

		if len(values) != info.request.actualColCount {
			return &Iter{err: fmt.Errorf("gocql: expected %d values send got %d", info.request.actualColCount, len(values))}
		}

		params.values = make([]queryValues, len(values))
		for i := 0; i < len(values); i++ {
			val, err := Marshal(info.request.columns[i].TypeInfo, values[i])
			if err != nil {
				return &Iter{err: err}
			}

			v := &params.values[i]
			v.value = val
			// TODO: handle query binding names
		}

		params.skipMeta = !qry.disableSkipMetadata

		frame = &writeExecuteFrame{
			preparedID: info.id,
			params:     params,
		}
	} else {
		frame = &writeQueryFrame{
			statement: qry.stmt,
			params:    params,
		}
	}

	framer, err := c.exec(frame, qry.trace)
	if err != nil {
		return &Iter{err: err}
	}

	resp, err := framer.parseFrame()
	if err != nil {
		return &Iter{err: err}
	}

	if len(framer.traceID) > 0 {
		qry.trace.Trace(framer.traceID)
	}

	switch x := resp.(type) {
	case *resultVoidFrame:
		return &Iter{framer: framer}
	case *resultRowsFrame:
		iter := &Iter{
			meta:    x.meta,
			framer:  framer,
			numRows: x.numRows,
		}

		if params.skipMeta {
			if info != nil {
				iter.meta = info.response
				iter.meta.pagingState = x.meta.pagingState
			} else {
				return &Iter{framer: framer, err: errors.New("gocql: did not receive metadata but prepared info is nil")}
			}
		} else {
			iter.meta = x.meta
		}

		if len(x.meta.pagingState) > 0 && !qry.disableAutoPage {
			iter.next = &nextIter{
				qry: *qry,
				pos: int((1 - qry.prefetch) * float64(x.numRows)),
			}

			iter.next.qry.pageState = copyBytes(x.meta.pagingState)
			if iter.next.pos < 1 {
				iter.next.pos = 1
			}
		}

		return iter
	case *resultKeyspaceFrame:
		return &Iter{framer: framer}
	case *schemaChangeKeyspace, *schemaChangeTable, *schemaChangeFunction:
		iter := &Iter{framer: framer}
		if err := c.awaitSchemaAgreement(); err != nil {
			// TODO: should have this behind a flag
			log.Println(err)
		}
		// dont return an error from this, might be a good idea to give a warning
		// though. The impact of this returning an error would be that the cluster
		// is not consistent with regards to its schema.
		return iter
	case *RequestErrUnprepared:
		stmtCacheKey := c.session.stmtsLRU.keyFor(c.addr, c.currentKeyspace, qry.stmt)
		if c.session.stmtsLRU.remove(stmtCacheKey) {
			return c.executeQuery(qry)
		}

		return &Iter{err: x, framer: framer}
	case error:
		return &Iter{err: x, framer: framer}
	default:
		return &Iter{
			err:    NewErrProtocol("Unknown type in response to execute query (%T): %s", x, x),
			framer: framer,
		}
	}
}

func (c *Conn) Pick(qry *Query) *Conn {
	if c.Closed() {
		return nil
	}
	return c
}

func (c *Conn) Closed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

func (c *Conn) Address() string {
	return c.addr
}

func (c *Conn) AvailableStreams() int {
	return c.streams.Available()
}

func (c *Conn) UseKeyspace(keyspace string) error {
	q := &writeQueryFrame{statement: `USE "` + keyspace + `"`}
	q.params.consistency = Any

	framer, err := c.exec(q, nil)
	if err != nil {
		return err
	}

	resp, err := framer.parseFrame()
	if err != nil {
		return err
	}

	switch x := resp.(type) {
	case *resultKeyspaceFrame:
	case error:
		return x
	default:
		return NewErrProtocol("unknown frame in response to USE: %v", x)
	}

	c.currentKeyspace = keyspace

	return nil
}

func (c *Conn) executeBatch(batch *Batch) *Iter {
	if c.version == protoVersion1 {
		return &Iter{err: ErrUnsupported}
	}

	n := len(batch.Entries)
	req := &writeBatchFrame{
		typ:               batch.Type,
		statements:        make([]batchStatment, n),
		consistency:       batch.Cons,
		serialConsistency: batch.serialCons,
		defaultTimestamp:  batch.defaultTimestamp,
	}

	stmts := make(map[string]string, len(batch.Entries))

	for i := 0; i < n; i++ {
		entry := &batch.Entries[i]
		b := &req.statements[i]
		if len(entry.Args) > 0 || entry.binding != nil {
			info, err := c.prepareStatement(entry.Stmt, nil)
			if err != nil {
				return &Iter{err: err}
			}

			var values []interface{}
			if entry.binding == nil {
				values = entry.Args
			} else {
				values, err = entry.binding(&QueryInfo{
					Id:          info.id,
					Args:        info.request.columns,
					Rval:        info.response.columns,
					PKeyColumns: info.request.pkeyColumns,
				})
				if err != nil {
					return &Iter{err: err}
				}
			}

			if len(values) != info.request.actualColCount {
				return &Iter{err: fmt.Errorf("gocql: batch statment %d expected %d values send got %d", i, info.request.actualColCount, len(values))}
			}

			b.preparedID = info.id
			stmts[string(info.id)] = entry.Stmt

			b.values = make([]queryValues, info.request.actualColCount)

			for j := 0; j < info.request.actualColCount; j++ {
				val, err := Marshal(info.request.columns[j].TypeInfo, values[j])
				if err != nil {
					return &Iter{err: err}
				}

				b.values[j].value = val
				// TODO: add names
			}
		} else {
			b.statement = entry.Stmt
		}
	}

	// TODO: should batch support tracing?
	framer, err := c.exec(req, nil)
	if err != nil {
		return &Iter{err: err}
	}

	resp, err := framer.parseFrame()
	if err != nil {
		return &Iter{err: err, framer: framer}
	}

	switch x := resp.(type) {
	case *resultVoidFrame:
		framerPool.Put(framer)
		return &Iter{}
	case *RequestErrUnprepared:
		stmt, found := stmts[string(x.StatementId)]
		if found {
			key := c.session.stmtsLRU.keyFor(c.addr, c.currentKeyspace, stmt)
			c.session.stmtsLRU.remove(key)
		}

		framerPool.Put(framer)

		if found {
			return c.executeBatch(batch)
		} else {
			return &Iter{err: err, framer: framer}
		}
	case *resultRowsFrame:
		iter := &Iter{
			meta:    x.meta,
			framer:  framer,
			numRows: x.numRows,
		}

		return iter
	case error:

		return &Iter{err: err, framer: framer}
	default:
		return &Iter{err: NewErrProtocol("Unknown type in response to batch statement: %s", x), framer: framer}
	}
}

func (c *Conn) setKeepalive(d time.Duration) error {
	if tc, ok := c.conn.(*net.TCPConn); ok {
		err := tc.SetKeepAlivePeriod(d)
		if err != nil {
			return err
		}

		return tc.SetKeepAlive(true)
	}

	return nil
}

func (c *Conn) query(statement string, values ...interface{}) (iter *Iter) {
	q := c.session.Query(statement, values...).Consistency(One)
	return c.executeQuery(q)
}

func (c *Conn) awaitSchemaAgreement() (err error) {
	const (
		peerSchemas  = "SELECT schema_version FROM system.peers"
		localSchemas = "SELECT schema_version FROM system.local WHERE key='local'"
	)

	var versions map[string]struct{}

	endDeadline := time.Now().Add(c.session.cfg.MaxWaitSchemaAgreement)
	for time.Now().Before(endDeadline) {
		iter := c.query(peerSchemas)

		versions = make(map[string]struct{})

		var schemaVersion string
		for iter.Scan(&schemaVersion) {
			versions[schemaVersion] = struct{}{}
			schemaVersion = ""
		}

		if err = iter.Close(); err != nil {
			goto cont
		}

		iter = c.query(localSchemas)
		for iter.Scan(&schemaVersion) {
			versions[schemaVersion] = struct{}{}
			schemaVersion = ""
		}

		if err = iter.Close(); err != nil {
			goto cont
		}

		if len(versions) <= 1 {
			return nil
		}

	cont:
		time.Sleep(200 * time.Millisecond)
	}

	if err != nil {
		return
	}

	schemas := make([]string, 0, len(versions))
	for schema := range versions {
		schemas = append(schemas, schema)
	}

	// not exported
	return fmt.Errorf("gocql: cluster schema versions not consistent: %+v", schemas)
}

var (
	ErrQueryArgLength    = errors.New("gocql: query argument length mismatch")
	ErrTimeoutNoResponse = errors.New("gocql: no response received from cassandra within timeout period")
	ErrTooManyTimeouts   = errors.New("gocql: too many query timeouts on the connection")
	ErrConnectionClosed  = errors.New("gocql: connection closed waiting for response")
	ErrNoStreams         = errors.New("gocql: no streams available on connection")
)
