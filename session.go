// Copyright (c) 2012 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gocql

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gocql/gocql/internal/lru"
)

// Session is the interface used by users to interact with the database.
//
// It's safe for concurrent use by multiple goroutines and a typical usage
// scenario is to have one global session object to interact with the
// whole Cassandra cluster.
//
// This type extends the Node interface by adding a convinient query builder
// and automatically sets a default consinstency level on all operations
// that do not have a consistency level set.
type Session struct {
	pool                *policyConnPool
	cons                Consistency
	pageSize            int
	prefetch            float64
	routingKeyInfoCache routingKeyInfoLRU
	schemaDescriber     *schemaDescriber
	trace               Tracer
	hostSource          *ringDescriber
	ring                ring
	stmtsLRU            *preparedLRU

	connCfg *ConnConfig

	mu sync.RWMutex

	control *controlConn

	// event handlers
	nodeEvents   *eventDeouncer
	schemaEvents *eventDeouncer

	// ring metadata
	hosts []HostInfo

	cfg ClusterConfig

	closeMu  sync.RWMutex
	isClosed bool
}

func addrsToHosts(addrs []string, defaultPort int) ([]*HostInfo, error) {
	hosts := make([]*HostInfo, len(addrs))
	for i, hostport := range addrs {
		// TODO: remove duplication
		addr, portStr, err := net.SplitHostPort(JoinHostPort(hostport, defaultPort))
		if err != nil {
			return nil, fmt.Errorf("NewSession: unable to parse hostport of addr %q: %v", hostport, err)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("NewSession: invalid port for hostport of addr %q: %v", hostport, err)
		}

		hosts[i] = &HostInfo{peer: addr, port: port, state: NodeUp}
	}

	return hosts, nil
}

// NewSession wraps an existing Node.
func NewSession(cfg ClusterConfig) (*Session, error) {
	//Check that hosts in the ClusterConfig is not empty
	if len(cfg.Hosts) < 1 {
		return nil, ErrNoHosts
	}

	s := &Session{
		cons:     cfg.Consistency,
		prefetch: 0.25,
		cfg:      cfg,
		pageSize: cfg.PageSize,
		stmtsLRU: &preparedLRU{lru: lru.New(cfg.MaxPreparedStmts)},
	}

	connCfg, err := connConfig(s)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("gocql: unable to create session: %v", err)
	}
	s.connCfg = connCfg

	s.nodeEvents = newEventDeouncer("NodeEvents", s.handleNodeEvent)
	s.schemaEvents = newEventDeouncer("SchemaEvents", s.handleSchemaEvent)

	s.routingKeyInfoCache.lru = lru.New(cfg.MaxRoutingKeyInfo)

	// I think it might be a good idea to simplify this and make it always discover
	// hosts, maybe with more filters.
	s.hostSource = &ringDescriber{
		session:   s,
		closeChan: make(chan bool),
	}

	s.pool = cfg.PoolConfig.buildPool(s)

	var hosts []*HostInfo

	if !cfg.disableControlConn {
		s.control = createControlConn(s)
		if err := s.control.connect(cfg.Hosts); err != nil {
			s.Close()
			return nil, err
		}

		// need to setup host source to check for broadcast_address in system.local
		localHasRPCAddr, _ := checkSystemLocal(s.control)
		s.hostSource.localHasRpcAddr = localHasRPCAddr

		var err error
		if cfg.DisableInitialHostLookup {
			// TODO: we could look at system.local to get token and other metadata
			// in this case.
			hosts, err = addrsToHosts(cfg.Hosts, cfg.Port)
		} else {
			hosts, _, err = s.hostSource.GetHosts()
		}

		if err != nil {
			s.Close()
			return nil, err
		}
	} else {
		// we dont get host info
		hosts, err = addrsToHosts(cfg.Hosts, cfg.Port)
	}

	for _, host := range hosts {
		if s.cfg.HostFilter == nil || s.cfg.HostFilter.Accept(host) {
			s.ring.addHost(host)
			s.handleNodeUp(net.ParseIP(host.Peer()), host.Port(), false)
		}
	}

	// TODO(zariel): we probably dont need this any more as we verify that we
	// can connect to one of the endpoints supplied by using the control conn.
	// See if there are any connections in the pool
	if s.pool.Size() == 0 {
		s.Close()
		return nil, ErrNoConnectionsStarted
	}

	return s, nil
}

// SetConsistency sets the default consistency level for this session. This
// setting can also be changed on a per-query basis and the default value
// is Quorum.
func (s *Session) SetConsistency(cons Consistency) {
	s.mu.Lock()
	s.cons = cons
	s.mu.Unlock()
}

// SetPageSize sets the default page size for this session. A value <= 0 will
// disable paging. This setting can also be changed on a per-query basis.
func (s *Session) SetPageSize(n int) {
	s.mu.Lock()
	s.pageSize = n
	s.mu.Unlock()
}

// SetPrefetch sets the default threshold for pre-fetching new pages. If
// there are only p*pageSize rows remaining, the next page will be requested
// automatically. This value can also be changed on a per-query basis and
// the default value is 0.25.
func (s *Session) SetPrefetch(p float64) {
	s.mu.Lock()
	s.prefetch = p
	s.mu.Unlock()
}

// SetTrace sets the default tracer for this session. This setting can also
// be changed on a per-query basis.
func (s *Session) SetTrace(trace Tracer) {
	s.mu.Lock()
	s.trace = trace
	s.mu.Unlock()
}

// Query generates a new query object for interacting with the database.
// Further details of the query may be tweaked using the resulting query
// value before the query is executed. Query is automatically prepared
// if it has not previously been executed.
func (s *Session) Query(stmt string, values ...interface{}) *Query {
	s.mu.RLock()
	qry := &Query{stmt: stmt, values: values, cons: s.cons,
		session: s, pageSize: s.pageSize, trace: s.trace,
		prefetch: s.prefetch, rt: s.cfg.RetryPolicy, serialCons: s.cfg.SerialConsistency,
		defaultTimestamp: s.cfg.DefaultTimestamp,
	}
	s.mu.RUnlock()
	return qry
}

type QueryInfo struct {
	Id          []byte
	Args        []ColumnInfo
	Rval        []ColumnInfo
	PKeyColumns []int
}

// Bind generates a new query object based on the query statement passed in.
// The query is automatically prepared if it has not previously been executed.
// The binding callback allows the application to define which query argument
// values will be marshalled as part of the query execution.
// During execution, the meta data of the prepared query will be routed to the
// binding callback, which is responsible for producing the query argument values.
func (s *Session) Bind(stmt string, b func(q *QueryInfo) ([]interface{}, error)) *Query {
	s.mu.RLock()
	qry := &Query{stmt: stmt, binding: b, cons: s.cons,
		session: s, pageSize: s.pageSize, trace: s.trace,
		prefetch: s.prefetch, rt: s.cfg.RetryPolicy}
	s.mu.RUnlock()
	return qry
}

// Close closes all connections. The session is unusable after this
// operation.
func (s *Session) Close() {

	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.isClosed {
		return
	}
	s.isClosed = true

	if s.pool != nil {
		s.pool.Close()
	}

	if s.hostSource != nil {
		close(s.hostSource.closeChan)
	}

	if s.control != nil {
		s.control.close()
	}

	if s.nodeEvents != nil {
		s.nodeEvents.stop()
	}

	if s.schemaEvents != nil {
		s.schemaEvents.stop()
	}
}

func (s *Session) Closed() bool {
	s.closeMu.RLock()
	closed := s.isClosed
	s.closeMu.RUnlock()
	return closed
}

func (s *Session) executeQuery(qry *Query) *Iter {

	// fail fast
	if s.Closed() {
		return &Iter{err: ErrSessionClosed}
	}

	var iter *Iter
	qry.attempts = 0
	qry.totalLatency = 0
	for {
		host, conn := s.pool.Pick(qry)

		qry.attempts++
		//Assign the error unavailable to the iterator
		if conn == nil {
			if qry.rt == nil || !qry.rt.Attempt(qry) {
				iter = &Iter{err: ErrNoConnections}
				break
			}

			continue
		}

		t := time.Now()
		iter = conn.executeQuery(qry)
		qry.totalLatency += time.Now().Sub(t).Nanoseconds()

		// Update host
		host.Mark(iter.err)

		// Exit for loop if the query was successful
		if iter.err == nil {
			break
		}

		if qry.rt == nil || !qry.rt.Attempt(qry) {
			break
		}
	}

	return iter
}

// KeyspaceMetadata returns the schema metadata for the keyspace specified.
func (s *Session) KeyspaceMetadata(keyspace string) (*KeyspaceMetadata, error) {
	// fail fast
	if s.Closed() {
		return nil, ErrSessionClosed
	}

	if keyspace == "" {
		return nil, ErrNoKeyspace
	}

	s.mu.Lock()
	// lazy-init schemaDescriber
	if s.schemaDescriber == nil {
		s.schemaDescriber = newSchemaDescriber(s)
	}
	s.mu.Unlock()

	return s.schemaDescriber.getSchema(keyspace)
}

// returns routing key indexes and type info
func (s *Session) routingKeyInfo(stmt string) (*routingKeyInfo, error) {
	s.routingKeyInfoCache.mu.Lock()

	entry, cached := s.routingKeyInfoCache.lru.Get(stmt)
	if cached {
		// done accessing the cache
		s.routingKeyInfoCache.mu.Unlock()
		// the entry is an inflight struct similiar to that used by
		// Conn to prepare statements
		inflight := entry.(*inflightCachedEntry)

		// wait for any inflight work
		inflight.wg.Wait()

		if inflight.err != nil {
			return nil, inflight.err
		}

		key, _ := inflight.value.(*routingKeyInfo)

		return key, nil
	}

	// create a new inflight entry while the data is created
	inflight := new(inflightCachedEntry)
	inflight.wg.Add(1)
	defer inflight.wg.Done()
	s.routingKeyInfoCache.lru.Add(stmt, inflight)
	s.routingKeyInfoCache.mu.Unlock()

	var (
		info         *preparedStatment
		partitionKey []*ColumnMetadata
	)

	// get the query info for the statement
	host, conn := s.pool.Pick(nil)
	if conn == nil {
		// no connections
		inflight.err = ErrNoConnections
		// don't cache this error
		s.routingKeyInfoCache.Remove(stmt)
		return nil, inflight.err
	}

	info, inflight.err = conn.prepareStatement(stmt, nil)
	if inflight.err != nil {
		// don't cache this error
		s.routingKeyInfoCache.Remove(stmt)
		host.Mark(inflight.err)
		return nil, inflight.err
	}

	// Mark host as OK
	host.Mark(nil)

	if info.request.colCount == 0 {
		// no arguments, no routing key, and no error
		return nil, nil
	}

	// get the table metadata
	table := info.request.columns[0].Table

	var keyspaceMetadata *KeyspaceMetadata
	keyspaceMetadata, inflight.err = s.KeyspaceMetadata(s.cfg.Keyspace)
	if inflight.err != nil {
		// don't cache this error
		s.routingKeyInfoCache.Remove(stmt)
		return nil, inflight.err
	}

	tableMetadata, found := keyspaceMetadata.Tables[table]
	if !found {
		// unlikely that the statement could be prepared and the metadata for
		// the table couldn't be found, but this may indicate either a bug
		// in the metadata code, or that the table was just dropped.
		inflight.err = ErrNoMetadata
		// don't cache this error
		s.routingKeyInfoCache.Remove(stmt)
		return nil, inflight.err
	}

	partitionKey = tableMetadata.PartitionKey

	size := len(partitionKey)
	routingKeyInfo := &routingKeyInfo{
		indexes: make([]int, size),
		types:   make([]TypeInfo, size),
	}
	for keyIndex, keyColumn := range partitionKey {
		// set an indicator for checking if the mapping is missing
		routingKeyInfo.indexes[keyIndex] = -1

		// find the column in the query info
		for argIndex, boundColumn := range info.request.columns {
			if keyColumn.Name == boundColumn.Name {
				// there may be many such bound columns, pick the first
				routingKeyInfo.indexes[keyIndex] = argIndex
				routingKeyInfo.types[keyIndex] = boundColumn.TypeInfo
				break
			}
		}

		if routingKeyInfo.indexes[keyIndex] == -1 {
			// missing a routing key column mapping
			// no routing key, and no error
			return nil, nil
		}
	}

	// cache this result
	inflight.value = routingKeyInfo

	return routingKeyInfo, nil
}

func (s *Session) executeBatch(batch *Batch) *Iter {
	// fail fast
	if s.Closed() {
		return &Iter{err: ErrSessionClosed}
	}

	// Prevent the execution of the batch if greater than the limit
	// Currently batches have a limit of 65536 queries.
	// https://datastax-oss.atlassian.net/browse/JAVA-229
	if batch.Size() > BatchSizeMaximum {
		return &Iter{err: ErrTooManyStmts}
	}

	var iter *Iter
	batch.attempts = 0
	batch.totalLatency = 0
	for {
		host, conn := s.pool.Pick(nil)

		batch.attempts++
		if conn == nil {
			if batch.rt == nil || !batch.rt.Attempt(batch) {
				// Assign the error unavailable and break loop
				iter = &Iter{err: ErrNoConnections}
				break
			}

			continue
		}

		if conn == nil {
			iter = &Iter{err: ErrNoConnections}
			break
		}

		t := time.Now()

		iter = conn.executeBatch(batch)

		batch.totalLatency += time.Since(t).Nanoseconds()
		// Exit loop if operation executed correctly
		if iter.err == nil {
			host.Mark(nil)
			break
		}

		// Mark host with error if returned from Close
		host.Mark(iter.Close())

		if batch.rt == nil || !batch.rt.Attempt(batch) {
			break
		}
	}

	return iter
}

// ExecuteBatch executes a batch operation and returns nil if successful
// otherwise an error is returned describing the failure.
func (s *Session) ExecuteBatch(batch *Batch) error {
	iter := s.executeBatch(batch)
	return iter.Close()
}

// ExecuteBatchCAS executes a batch operation and returns true if successful and
// an iterator (to scan aditional rows if more than one conditional statement)
// was sent.
// Further scans on the interator must also remember to include
// the applied boolean as the first argument to *Iter.Scan
func (s *Session) ExecuteBatchCAS(batch *Batch, dest ...interface{}) (applied bool, iter *Iter, err error) {
	iter = s.executeBatch(batch)
	if err := iter.checkErrAndNotFound(); err != nil {
		iter.Close()
		return false, nil, err
	}

	if len(iter.Columns()) > 1 {
		dest = append([]interface{}{&applied}, dest...)
		iter.Scan(dest...)
	} else {
		iter.Scan(&applied)
	}

	return applied, iter, nil
}

// MapExecuteBatchCAS executes a batch operation much like ExecuteBatchCAS,
// however it accepts a map rather than a list of arguments for the initial
// scan.
func (s *Session) MapExecuteBatchCAS(batch *Batch, dest map[string]interface{}) (applied bool, iter *Iter, err error) {
	iter = s.executeBatch(batch)
	if err := iter.checkErrAndNotFound(); err != nil {
		iter.Close()
		return false, nil, err
	}
	iter.MapScan(dest)
	applied = dest["[applied]"].(bool)
	delete(dest, "[applied]")

	// we usually close here, but instead of closing, just returin an error
	// if MapScan failed. Although Close just returns err, using Close
	// here might be confusing as we are not actually closing the iter
	return applied, iter, iter.err
}

func (s *Session) connect(addr string, errorHandler ConnErrorHandler) (*Conn, error) {
	return Connect(addr, s.connCfg, errorHandler, s)
}

// Query represents a CQL statement that can be executed.
type Query struct {
	stmt                string
	values              []interface{}
	cons                Consistency
	pageSize            int
	routingKey          []byte
	routingKeyBuffer    []byte
	pageState           []byte
	prefetch            float64
	trace               Tracer
	session             *Session
	rt                  RetryPolicy
	binding             func(q *QueryInfo) ([]interface{}, error)
	attempts            int
	totalLatency        int64
	serialCons          SerialConsistency
	defaultTimestamp    bool
	disableSkipMetadata bool

	disableAutoPage bool
}

// String implements the stringer interface.
func (q Query) String() string {
	return fmt.Sprintf("[query statement=%q values=%+v consistency=%s]", q.stmt, q.values, q.cons)
}

//Attempts returns the number of times the query was executed.
func (q *Query) Attempts() int {
	return q.attempts
}

//Latency returns the average amount of nanoseconds per attempt of the query.
func (q *Query) Latency() int64 {
	if q.attempts > 0 {
		return q.totalLatency / int64(q.attempts)
	}
	return 0
}

// Consistency sets the consistency level for this query. If no consistency
// level have been set, the default consistency level of the cluster
// is used.
func (q *Query) Consistency(c Consistency) *Query {
	q.cons = c
	return q
}

// GetConsistency returns the currently configured consistency level for
// the query.
func (q *Query) GetConsistency() Consistency {
	return q.cons
}

// Trace enables tracing of this query. Look at the documentation of the
// Tracer interface to learn more about tracing.
func (q *Query) Trace(trace Tracer) *Query {
	q.trace = trace
	return q
}

// PageSize will tell the iterator to fetch the result in pages of size n.
// This is useful for iterating over large result sets, but setting the
// page size to low might decrease the performance. This feature is only
// available in Cassandra 2 and onwards.
func (q *Query) PageSize(n int) *Query {
	q.pageSize = n
	return q
}

// DefaultTimestamp will enable the with default timestamp flag on the query.
// If enable, this will replace the server side assigned
// timestamp as default timestamp. Note that a timestamp in the query itself
// will still override this timestamp. This is entirely optional.
//
// Only available on protocol >= 3
func (q *Query) DefaultTimestamp(enable bool) *Query {
	q.defaultTimestamp = enable
	return q
}

// RoutingKey sets the routing key to use when a token aware connection
// pool is used to optimize the routing of this query.
func (q *Query) RoutingKey(routingKey []byte) *Query {
	q.routingKey = routingKey
	return q
}

// GetRoutingKey gets the routing key to use for routing this query. If
// a routing key has not been explicitly set, then the routing key will
// be constructed if possible using the keyspace's schema and the query
// info for this query statement. If the routing key cannot be determined
// then nil will be returned with no error. On any error condition,
// an error description will be returned.
func (q *Query) GetRoutingKey() ([]byte, error) {
	if q.routingKey != nil {
		return q.routingKey, nil
	}

	// try to determine the routing key
	routingKeyInfo, err := q.session.routingKeyInfo(q.stmt)
	if err != nil {
		return nil, err
	}

	if routingKeyInfo == nil {
		return nil, nil
	}

	if len(routingKeyInfo.indexes) == 1 {
		// single column routing key
		routingKey, err := Marshal(
			routingKeyInfo.types[0],
			q.values[routingKeyInfo.indexes[0]],
		)
		if err != nil {
			return nil, err
		}
		return routingKey, nil
	}

	// We allocate that buffer only once, so that further re-bind/exec of the
	// same query don't allocate more memory.
	if q.routingKeyBuffer == nil {
		q.routingKeyBuffer = make([]byte, 0, 256)
	}

	// composite routing key
	buf := bytes.NewBuffer(q.routingKeyBuffer)
	for i := range routingKeyInfo.indexes {
		encoded, err := Marshal(
			routingKeyInfo.types[i],
			q.values[routingKeyInfo.indexes[i]],
		)
		if err != nil {
			return nil, err
		}
		lenBuf := []byte{0x00, 0x00}
		binary.BigEndian.PutUint16(lenBuf, uint16(len(encoded)))
		buf.Write(lenBuf)
		buf.Write(encoded)
		buf.WriteByte(0x00)
	}
	routingKey := buf.Bytes()
	return routingKey, nil
}

func (q *Query) shouldPrepare() bool {

	stmt := strings.TrimLeftFunc(strings.TrimRightFunc(q.stmt, func(r rune) bool {
		return unicode.IsSpace(r) || r == ';'
	}), unicode.IsSpace)

	var stmtType string
	if n := strings.IndexFunc(stmt, unicode.IsSpace); n >= 0 {
		stmtType = strings.ToLower(stmt[:n])
	}
	if stmtType == "begin" {
		if n := strings.LastIndexFunc(stmt, unicode.IsSpace); n >= 0 {
			stmtType = strings.ToLower(stmt[n+1:])
		}
	}
	switch stmtType {
	case "select", "insert", "update", "delete", "batch":
		return true
	}
	return false
}

// SetPrefetch sets the default threshold for pre-fetching new pages. If
// there are only p*pageSize rows remaining, the next page will be requested
// automatically.
func (q *Query) Prefetch(p float64) *Query {
	q.prefetch = p
	return q
}

// RetryPolicy sets the policy to use when retrying the query.
func (q *Query) RetryPolicy(r RetryPolicy) *Query {
	q.rt = r
	return q
}

// Bind sets query arguments of query. This can also be used to rebind new query arguments
// to an existing query instance.
func (q *Query) Bind(v ...interface{}) *Query {
	q.values = v
	return q
}

// SerialConsistency sets the consistencyc level for the
// serial phase of conditional updates. That consitency can only be
// either SERIAL or LOCAL_SERIAL and if not present, it defaults to
// SERIAL. This option will be ignored for anything else that a
// conditional update/insert.
func (q *Query) SerialConsistency(cons SerialConsistency) *Query {
	q.serialCons = cons
	return q
}

// PageState sets the paging state for the query to resume paging from a specific
// point in time. Setting this will disable to query paging for this query, and
// must be used for all subsequent pages.
func (q *Query) PageState(state []byte) *Query {
	q.pageState = state
	q.disableAutoPage = true
	return q
}

// NoSkipMetadata will override the internal result metadata cache so that the driver does not
// send skip_metadata for queries, this means that the result will always contain
// the metadata to parse the rows and will not reuse the metadata from the prepared
// staement. This should only be used to work around cassandra bugs, such as when using
// CAS operations which do not end in Cas.
//
// See https://issues.apache.org/jira/browse/CASSANDRA-11099
// https://github.com/gocql/gocql/issues/612
func (q *Query) NoSkipMetadata() *Query {
	q.disableSkipMetadata = true
	return q
}

// Exec executes the query without returning any rows.
func (q *Query) Exec() error {
	iter := q.Iter()
	return iter.Close()
}

func isUseStatement(stmt string) bool {
	if len(stmt) < 3 {
		return false
	}

	return strings.ToLower(stmt[0:3]) == "use"
}

// Iter executes the query and returns an iterator capable of iterating
// over all results.
func (q *Query) Iter() *Iter {
	if isUseStatement(q.stmt) {
		return &Iter{err: ErrUseStmt}
	}
	return q.session.executeQuery(q)
}

// MapScan executes the query, copies the columns of the first selected
// row into the map pointed at by m and discards the rest. If no rows
// were selected, ErrNotFound is returned.
func (q *Query) MapScan(m map[string]interface{}) error {
	iter := q.Iter()
	if err := iter.checkErrAndNotFound(); err != nil {
		return err
	}
	iter.MapScan(m)
	return iter.Close()
}

// Scan executes the query, copies the columns of the first selected
// row into the values pointed at by dest and discards the rest. If no rows
// were selected, ErrNotFound is returned.
func (q *Query) Scan(dest ...interface{}) error {
	iter := q.Iter()
	if err := iter.checkErrAndNotFound(); err != nil {
		return err
	}
	iter.Scan(dest...)
	return iter.Close()
}

// ScanCAS executes a lightweight transaction (i.e. an UPDATE or INSERT
// statement containing an IF clause). If the transaction fails because
// the existing values did not match, the previous values will be stored
// in dest.
func (q *Query) ScanCAS(dest ...interface{}) (applied bool, err error) {
	q.disableSkipMetadata = true
	iter := q.Iter()
	if err := iter.checkErrAndNotFound(); err != nil {
		return false, err
	}
	if len(iter.Columns()) > 1 {
		dest = append([]interface{}{&applied}, dest...)
		iter.Scan(dest...)
	} else {
		iter.Scan(&applied)
	}
	return applied, iter.Close()
}

// MapScanCAS executes a lightweight transaction (i.e. an UPDATE or INSERT
// statement containing an IF clause). If the transaction fails because
// the existing values did not match, the previous values will be stored
// in dest map.
//
// As for INSERT .. IF NOT EXISTS, previous values will be returned as if
// SELECT * FROM. So using ScanCAS with INSERT is inherently prone to
// column mismatching. MapScanCAS is added to capture them safely.
func (q *Query) MapScanCAS(dest map[string]interface{}) (applied bool, err error) {
	q.disableSkipMetadata = true
	iter := q.Iter()
	if err := iter.checkErrAndNotFound(); err != nil {
		return false, err
	}
	iter.MapScan(dest)
	applied = dest["[applied]"].(bool)
	delete(dest, "[applied]")

	return applied, iter.Close()
}

// Iter represents an iterator that can be used to iterate over all rows that
// were returned by a query. The iterator might send additional queries to the
// database during the iteration if paging was enabled.
type Iter struct {
	err     error
	pos     int
	meta    resultMetadata
	numRows int
	next    *nextIter
	host    *HostInfo

	framer *framer
	once   sync.Once
}

// Host returns the host which the query was sent to.
func (iter *Iter) Host() *HostInfo {
	return iter.host
}

// Columns returns the name and type of the selected columns.
func (iter *Iter) Columns() []ColumnInfo {
	return iter.meta.columns
}

func (iter *Iter) readColumn() ([]byte, error) {
	return iter.framer.readBytesInternal()
}

// Scan consumes the next row of the iterator and copies the columns of the
// current row into the values pointed at by dest. Use nil as a dest value
// to skip the corresponding column. Scan might send additional queries
// to the database to retrieve the next set of rows if paging was enabled.
//
// Scan returns true if the row was successfully unmarshaled or false if the
// end of the result set was reached or if an error occurred. Close should
// be called afterwards to retrieve any potential errors.
func (iter *Iter) Scan(dest ...interface{}) bool {
	if iter.err != nil {
		return false
	}

	if iter.pos >= iter.numRows {
		if iter.next != nil {
			*iter = *iter.next.fetch()
			return iter.Scan(dest...)
		}
		return false
	}

	if iter.next != nil && iter.pos == iter.next.pos {
		go iter.next.fetch()
	}

	// currently only support scanning into an expand tuple, such that its the same
	// as scanning in more values from a single column
	if len(dest) != iter.meta.actualColCount {
		iter.err = fmt.Errorf("gocql: not enough columns to scan into: have %d want %d", len(dest), iter.meta.actualColCount)
		return false
	}

	// i is the current position in dest, could posible replace it and just use
	// slices of dest
	i := 0
	for c := range iter.meta.columns {
		col := &iter.meta.columns[c]
		colBytes, err := iter.readColumn()
		if err != nil {
			iter.err = err
			return false
		}

		if dest[i] == nil {
			i++
			continue
		}

		switch col.TypeInfo.Type() {
		case TypeTuple:
			// this will panic, actually a bug, please report
			tuple := col.TypeInfo.(TupleTypeInfo)

			count := len(tuple.Elems)
			// here we pass in a slice of the struct which has the number number of
			// values as elements in the tuple
			iter.err = Unmarshal(col.TypeInfo, colBytes, dest[i:i+count])
			i += count
		default:
			iter.err = Unmarshal(col.TypeInfo, colBytes, dest[i])
			i++
		}

		if iter.err != nil {
			return false
		}
	}

	iter.pos++
	return true
}

// Close closes the iterator and returns any errors that happened during
// the query or the iteration.
func (iter *Iter) Close() error {
	iter.once.Do(func() {
		if iter.framer != nil {
			framerPool.Put(iter.framer)
			iter.framer = nil
		}
	})

	return iter.err
}

// WillSwitchPage detects if iterator reached end of current page
// and the next page is available.
func (iter *Iter) WillSwitchPage() bool {
	return iter.pos >= iter.numRows && iter.next != nil
}

// checkErrAndNotFound handle error and NotFound in one method.
func (iter *Iter) checkErrAndNotFound() error {
	if iter.err != nil {
		return iter.err
	} else if iter.numRows == 0 {
		return ErrNotFound
	}
	return nil
}

// PageState return the current paging state for a query which can be used for
// subsequent quries to resume paging this point.
func (iter *Iter) PageState() []byte {
	return iter.meta.pagingState
}

type nextIter struct {
	qry  Query
	pos  int
	once sync.Once
	next *Iter
}

func (n *nextIter) fetch() *Iter {
	n.once.Do(func() {
		n.next = n.qry.session.executeQuery(&n.qry)
	})
	return n.next
}

type Batch struct {
	Type             BatchType
	Entries          []BatchEntry
	Cons             Consistency
	rt               RetryPolicy
	attempts         int
	totalLatency     int64
	serialCons       SerialConsistency
	defaultTimestamp bool
}

// NewBatch creates a new batch operation without defaults from the cluster
func NewBatch(typ BatchType) *Batch {
	return &Batch{Type: typ}
}

// NewBatch creates a new batch operation using defaults defined in the cluster
func (s *Session) NewBatch(typ BatchType) *Batch {
	s.mu.RLock()
	batch := &Batch{Type: typ, rt: s.cfg.RetryPolicy, serialCons: s.cfg.SerialConsistency,
		Cons: s.cons, defaultTimestamp: s.cfg.DefaultTimestamp}
	s.mu.RUnlock()
	return batch
}

// Attempts returns the number of attempts made to execute the batch.
func (b *Batch) Attempts() int {
	return b.attempts
}

//Latency returns the average number of nanoseconds to execute a single attempt of the batch.
func (b *Batch) Latency() int64 {
	if b.attempts > 0 {
		return b.totalLatency / int64(b.attempts)
	}
	return 0
}

// GetConsistency returns the currently configured consistency level for the batch
// operation.
func (b *Batch) GetConsistency() Consistency {
	return b.Cons
}

// Query adds the query to the batch operation
func (b *Batch) Query(stmt string, args ...interface{}) {
	b.Entries = append(b.Entries, BatchEntry{Stmt: stmt, Args: args})
}

// Bind adds the query to the batch operation and correlates it with a binding callback
// that will be invoked when the batch is executed. The binding callback allows the application
// to define which query argument values will be marshalled as part of the batch execution.
func (b *Batch) Bind(stmt string, bind func(q *QueryInfo) ([]interface{}, error)) {
	b.Entries = append(b.Entries, BatchEntry{Stmt: stmt, binding: bind})
}

// RetryPolicy sets the retry policy to use when executing the batch operation
func (b *Batch) RetryPolicy(r RetryPolicy) *Batch {
	b.rt = r
	return b
}

// Size returns the number of batch statements to be executed by the batch operation.
func (b *Batch) Size() int {
	return len(b.Entries)
}

// SerialConsistency sets the consistencyc level for the
// serial phase of conditional updates. That consitency can only be
// either SERIAL or LOCAL_SERIAL and if not present, it defaults to
// SERIAL. This option will be ignored for anything else that a
// conditional update/insert.
//
// Only available for protocol 3 and above
func (b *Batch) SerialConsistency(cons SerialConsistency) *Batch {
	b.serialCons = cons
	return b
}

// DefaultTimestamp will enable the with default timestamp flag on the query.
// If enable, this will replace the server side assigned
// timestamp as default timestamp. Note that a timestamp in the query itself
// will still override this timestamp. This is entirely optional.
//
// Only available on protocol >= 3
func (b *Batch) DefaultTimestamp(enable bool) *Batch {
	b.defaultTimestamp = enable
	return b
}

type BatchType byte

const (
	LoggedBatch   BatchType = 0
	UnloggedBatch BatchType = 1
	CounterBatch  BatchType = 2
)

type BatchEntry struct {
	Stmt    string
	Args    []interface{}
	binding func(q *QueryInfo) ([]interface{}, error)
}

type ColumnInfo struct {
	Keyspace string
	Table    string
	Name     string
	TypeInfo TypeInfo
}

func (c ColumnInfo) String() string {
	return fmt.Sprintf("[column keyspace=%s table=%s name=%s type=%v]", c.Keyspace, c.Table, c.Name, c.TypeInfo)
}

// routing key indexes LRU cache
type routingKeyInfoLRU struct {
	lru *lru.Cache
	mu  sync.Mutex
}

type routingKeyInfo struct {
	indexes []int
	types   []TypeInfo
}

func (r *routingKeyInfoLRU) Remove(key string) {
	r.mu.Lock()
	r.lru.Remove(key)
	r.mu.Unlock()
}

//Max adjusts the maximum size of the cache and cleans up the oldest records if
//the new max is lower than the previous value. Not concurrency safe.
func (r *routingKeyInfoLRU) Max(max int) {
	r.mu.Lock()
	for r.lru.Len() > max {
		r.lru.RemoveOldest()
	}
	r.lru.MaxEntries = max
	r.mu.Unlock()
}

type inflightCachedEntry struct {
	wg    sync.WaitGroup
	err   error
	value interface{}
}

// Tracer is the interface implemented by query tracers. Tracers have the
// ability to obtain a detailed event log of all events that happened during
// the execution of a query from Cassandra. Gathering this information might
// be essential for debugging and optimizing queries, but this feature should
// not be used on production systems with very high load.
type Tracer interface {
	Trace(traceId []byte)
}

type traceWriter struct {
	session *Session
	w       io.Writer
	mu      sync.Mutex
}

// NewTraceWriter returns a simple Tracer implementation that outputs
// the event log in a textual format.
func NewTraceWriter(session *Session, w io.Writer) Tracer {
	return &traceWriter{session: session, w: w}
}

func (t *traceWriter) Trace(traceId []byte) {
	var (
		coordinator string
		duration    int
	)
	iter := t.session.control.query(`SELECT coordinator, duration
			FROM system_traces.sessions
			WHERE session_id = ?`, traceId)

	iter.Scan(&coordinator, &duration)
	if err := iter.Close(); err != nil {
		t.mu.Lock()
		fmt.Fprintln(t.w, "Error:", err)
		t.mu.Unlock()
		return
	}

	var (
		timestamp time.Time
		activity  string
		source    string
		elapsed   int
	)

	fmt.Fprintf(t.w, "Tracing session %016x (coordinator: %s, duration: %v):\n",
		traceId, coordinator, time.Duration(duration)*time.Microsecond)

	t.mu.Lock()
	defer t.mu.Unlock()

	iter = t.session.control.query(`SELECT event_id, activity, source, source_elapsed
			FROM system_traces.events
			WHERE session_id = ?`, traceId)

	for iter.Scan(&timestamp, &activity, &source, &elapsed) {
		fmt.Fprintf(t.w, "%s: %s (source: %s, elapsed: %d)\n",
			timestamp.Format("2006/01/02 15:04:05.999999"), activity, source, elapsed)
	}

	if err := iter.Close(); err != nil {
		fmt.Fprintln(t.w, "Error:", err)
	}
}

type Error struct {
	Code    int
	Message string
}

func (e Error) Error() string {
	return e.Message
}

var (
	ErrNotFound      = errors.New("not found")
	ErrUnavailable   = errors.New("unavailable")
	ErrUnsupported   = errors.New("feature not supported")
	ErrTooManyStmts  = errors.New("too many statements")
	ErrUseStmt       = errors.New("use statements aren't supported. Please see https://github.com/gocql/gocql for explaination.")
	ErrSessionClosed = errors.New("session has been closed")
	ErrNoConnections = errors.New("no connections available")
	ErrNoKeyspace    = errors.New("no keyspace provided")
	ErrNoMetadata    = errors.New("no metadata available")
)

type ErrProtocol struct{ error }

func NewErrProtocol(format string, args ...interface{}) error {
	return ErrProtocol{fmt.Errorf(format, args...)}
}

// BatchSizeMaximum is the maximum number of statements a batch operation can have.
// This limit is set by cassandra and could change in the future.
const BatchSizeMaximum = 65535
