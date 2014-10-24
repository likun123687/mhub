package broker

import (
	log "github.com/funkygao/log4go"
	"github.com/funkygao/mhub/config"
	proto "github.com/funkygao/mqttmsg"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// An incomingConn represents a MQTT connection into a Server.
type incomingConn struct {
	server *Server

	flag *proto.Connect // nil if not CONNECT ok

	alive         bool
	conn          net.Conn
	jobs          chan job
	heartbeatStop chan struct{}
	store         Store
	lastOpTime    int64 // // Last Unix timestamp when recieved message from this conn
}

func (this *incomingConn) String() string {
	if this.flag == nil {
		// CONNECT not sent yet
		return this.conn.RemoteAddr().String()
	}

	return this.flag.ClientId + "@" + this.conn.RemoteAddr().String()
}

func (this *incomingConn) refreshOpTime() {
	atomic.StoreInt64(&this.lastOpTime, time.Now().Unix())
}

func (this *incomingConn) heartbeat(keepAliveTimer time.Duration) {
	ticker := time.NewTicker(keepAliveTimer)
	defer func() {
		ticker.Stop()
		log.Debug("%s hearbeat stopped", this)
	}()

	for {
		select {
		case <-ticker.C:
			// 1.5*KeepAliveTimer latency tolerance
			deadline := int64(float64(this.lastOpTime) + keepAliveTimer.Seconds()*1.5)
			overIdle := time.Now().Unix() - deadline
			if overIdle > 0 && this.alive {
				this.submitSync(&proto.Disconnect{}).wait()
				log.Warn("%s over idle %ds, kicked out", this, overIdle)

				this.server.stats.aborted()

				if this.flag != nil && this.flag.WillFlag {
					// TODO broker will publish a message on behalf of the client
				}

				return
			}

		case <-this.heartbeatStop:
			return
		}
	}

}

func (this *incomingConn) connected() bool {
	return this.flag != nil
}

func (this *incomingConn) add() *incomingConn {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	existing, present := clients[this.flag.ClientId]
	if present {
		return existing
	}

	clients[this.flag.ClientId] = this
	return nil
}

// Delete a connection; the conection must be closed by the caller first.
func (this *incomingConn) del() {
	clientsMu.Lock()
	if this.flag != nil {
		delete(clients, this.flag.ClientId)
	}
	clientsMu.Unlock()
}

func (this *incomingConn) submit(m proto.Message) {
	if !this.alive {
		log.Debug("%s submit on dead client: %T %+v", this, m, m)
		return
	}

	if this.server.cf.Broker.BuffOverflowStrategy == config.BufferOverflowBlock {
		this.jobs <- job{m: m}
		return
	}

	// config.BufferOverflowDiscard
	select {
	case this.jobs <- job{m: m}:
	default:
		log.Debug("client[%s]: outbound(%d) full, discard %T", this,
			len(this.jobs), m)
	}
}

// Queue a message, returns a channel that will be readable
// when the message is sent.
func (this *incomingConn) submitSync(m proto.Message) receipt {
	j := job{m: m, r: make(receipt)}
	this.jobs <- j
	return j.r
}

// race:
// client disconnect -> inboundLoop close jobs ->
// outboundLoop stops -> outboundLoop close conn, unsubAll, del
func (this *incomingConn) inboundLoop() {
	defer func() {
		this.server.stats.clientDisconnect()
		this.store.Close()

		this.alive = false // to avoid send on closed channel subs.c.submit FIXME
		close(this.jobs)   // will terminate outboundLoop
	}()

	for {
		// FIXME client connected, but idle for long, should kill it
		m, err := proto.DecodeOneMessage(this.conn, nil)
		if err != nil {
			if err != io.EOF && !strings.HasSuffix(err.Error(), errTcpUseOfClosedNetwork) {
				// e,g. read tcp 106.49.97.242:62547: connection reset by peer
				// e,g. read tcp 127.0.0.1:65256: operation timed out
				log.Error("%v: %s", err, this)
			}

			this.server.stats.aborted()

			return
		}

		this.server.stats.messageRecv()
		this.server.stats.addIn(m)
		this.refreshOpTime()

		if this.server.cf.Broker.Echo {
			log.Debug("%s -> %T %+v", this, m, m)
		}

		switch m := m.(type) {
		case *proto.Connect: // TODO close conn if too long no Connect
			rc := this.doConnect(m)

			// close connection if it was a bad connect
			if rc != proto.RetCodeAccepted {
				log.Error("%v: %s", proto.ConnectionErrors[rc], this)
				return
			}

			// connect ok
			log.Debug("new client: %s (c^%v, k^%v)",
				this, m.CleanSession, m.KeepAliveTimer)

		case *proto.Publish:
			this.doPublish(m)

		case *proto.Subscribe:
			this.doSubscribe(m)

		case *proto.Unsubscribe:
			this.doUnsubscribe(m)

		case *proto.PubAck:
			this.doPublishAck(m)

		case *proto.PingReq:
			// broker will never ping client
			this.validateMessage(m)
			this.submit(&proto.PingResp{})

		case *proto.Disconnect:
			log.Debug("%s actively disconnect", this)
			return

		default:
			log.Warn("%s -> unexpected %T", this, m)
			return
		}
	}
}

func (this *incomingConn) outboundLoop() {
	defer func() {
		// close connection on exit in order to cause inboundLoop to exit.
		// only outboundLoop can close conn,
		// otherwise outboundLoop will error: use of closed network connection
		log.Debug("%s conn closed", this)
		close(this.heartbeatStop)
		this.conn.Close()

		this.del()
		this.server.subs.unsubAll(this)
	}()

	var (
		t1      time.Time
		elapsed time.Duration
		totalN  int64
		slowN   int64
	)
	for {
		select {
		case job, on := <-this.jobs:
			if !on {
				// jobs chan was closed by inboundLoop
				return
			}

			if this.server.cf.Broker.Echo {
				log.Debug("%s <- %T %+v", this, job.m, job.m)
			}

			persist_outbound(this.store, job.m)

			t1 = time.Now()
			this.conn.SetWriteDeadline(t1.Add(this.server.cf.Broker.IOTimeout))
			err := job.m.Encode(this.conn)
			elapsed = time.Since(t1)
			if job.r != nil {
				// notifiy the sender that this message is sent
				close(job.r)
			}
			if err != nil {
				// e,g. write tcp 127.0.0.1:59919: connection reset by peer
				// e,g. write tcp 106.49.97.242:4341: broken pipe
				// e,g. write tcp 106.49.97.242:61016: i/o timeout
				// try:
				//     sock.write('foo')
				// except:
				//     pass # connection reset by peer
				// sock.write('bar') # broken pipe
				log.Error("client[%s]: %s, %s", this, err, elapsed)
				return
			}

			totalN++
			if elapsed.Nanoseconds() > this.server.cf.Broker.ClientSlowThreshold.Nanoseconds() {
				slowN++
				log.Warn("Slow client[%s] %d/%d, %s", this, slowN, totalN, elapsed)
			}

			this.server.stats.messageSend()
			this.server.stats.addOut(job.m)
			this.refreshOpTime()

			if _, ok := job.m.(*proto.Disconnect); ok {
				return
			}
		}
	}

}

// TODO
func (this *incomingConn) validateMessage(m proto.Message) {
	// must CONNECT before other methods
}

// TODO
func (this *incomingConn) nextInternalMsgId() {
	//this.server.cf.Peers.SelfId
}

func (this *incomingConn) doConnect(m *proto.Connect) (rc proto.ReturnCode) {
	rc = proto.RetCodeAccepted // default is ok

	// validate protocol name and version
	if m.ProtocolName != protocolName ||
		m.ProtocolVersion != protocolVersion {
		log.Error("invalid connection[%s] protocol %s, version %d",
			this, m.ProtocolName, m.ProtocolVersion)
		rc = proto.RetCodeUnacceptableProtocolVersion
	}

	// validate client id length
	if len(m.ClientId) < 1 || len(m.ClientId) > maxClientIdLength {
		rc = proto.RetCodeIdentifierRejected
	}
	this.flag = m // connection flag

	// authentication
	if !this.server.cf.Broker.AllowAnonymousConnect &&
		(!m.UsernameFlag || m.Username == "" ||
			!m.PasswordFlag || m.Password == "") {
		rc = proto.RetCodeNotAuthorized
	} else if m.UsernameFlag && !this.authenticate(m.Username, m.Password) {
		rc = proto.RetCodeBadUsernameOrPassword
	}

	// validate clientId should be udid TODO

	if this.server.cf.Broker.MaxConnections > 0 &&
		this.server.stats.Clients() > this.server.cf.Broker.MaxConnections {
		rc = proto.RetCodeServerUnavailable
	}

	// Disconnect existing connections.
	if existing := this.add(); existing != nil {
		log.Warn("found dup client: %s", this)

		// force disconnect existing client
		existing.submitSync(&proto.Disconnect{}).wait()
		existing.del()
	}
	this.add()

	if m.KeepAliveTimer > 0 {
		go this.heartbeat(time.Duration(m.KeepAliveTimer) * time.Second)
	}

	// TODO: Last will
	// The will option allows clients to prepare for the worst.
	if !m.CleanSession {
		// broker will keep the subscription active even after the client disconnects
		// It will also queue any new messages it receives for the client, but
		// only if they have QoS>0
		// restore client's subscriptions
		// deliver flying messages TODO
		// deliver on connect
	}

	this.submit(&proto.ConnAck{ReturnCode: rc})

	return
}

func (this *incomingConn) doPublish(m *proto.Publish) {
	this.validateMessage(m)

	// TODO assert m.TopicName is not wildcard
	persist_inbound(this.store, m)

	// replicate message to all subscribers of this topic
	this.server.subs.submit(m)

	// replication to peers
	if isGlobalTopic(m.TopicName) {
		this.server.peers.submit(m)
	}

	// for QoS 0, we need do nothing
	if m.Header.QosLevel == proto.QosAtLeastOnce { // QoS 1
		if m.MessageId == 0 {
			log.Error("client[%s] invalid message id", this)
		}

		this.submit(&proto.PubAck{MessageId: m.MessageId})
	}

	// retry-until-acknowledged

	// if a PUBLISH not authorized, MQTT has no way of telling client about this
	// it must always make a positive acknowledgement according to QoS

	if m.Retain {

	}
}

func (this *incomingConn) doPublishAck(m *proto.PubAck) {
	this.validateMessage(m)

	// get flying messages for this client
	// if not found, ignore this PubAck
	// if found, mark this flying message
}

func (this *incomingConn) doSubscribe(m *proto.Subscribe) {
	this.validateMessage(m)

	// The SUBSCRIBE message also specifies the QoS level at which the subscriber wants to receive published messages.

	suback := &proto.SubAck{
		MessageId: m.MessageId,
		TopicsQos: make([]proto.QosLevel, len(m.Topics)),
	}
	for i, tq := range m.Topics {
		// TODO: Handle varying QoS correctly
		this.server.subs.add(tq.Topic, this)

		suback.TopicsQos[i] = proto.QosAtMostOnce
	}
	this.submit(suback)

	// A server may start sending PUBLISH messages due to the subscription before the client receives the SUBACK message.

	// Note that if a server implementation does not authorize a SUBSCRIBE request to be made by a client, it has no way of informing that client. It must therefore make a positive acknowledgement with a SUBACK, and the client will not be informed that it was not authorized to subscribe.

	// Process retained messages
	for _, tq := range m.Topics {
		this.server.subs.sendRetain(tq.Topic, this)
	}
}

func (this *incomingConn) doUnsubscribe(m *proto.Unsubscribe) {
	this.validateMessage(m)

	for _, t := range m.Topics {
		this.server.subs.unsub(t, this)
	}

	this.submit(&proto.UnsubAck{MessageId: m.MessageId})
}
