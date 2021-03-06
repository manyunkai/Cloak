package multiplex

import (
	"errors"
	log "github.com/sirupsen/logrus"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
)

const (
	FIXED_CONN_MAPPING switchboardStrategy = iota
	UNIFORM_SPREAD
)

type switchboardConfig struct {
	Valve
	strategy switchboardStrategy
}

// switchboard is responsible for keeping the reference of TCP connections between client and server
type switchboard struct {
	session *Session

	*switchboardConfig

	conns      sync.Map
	nextConnId uint32

	broken uint32
}

func makeSwitchboard(sesh *Session, config *switchboardConfig) *switchboard {
	// rates are uint64 because in the usermanager we want the bandwidth to be atomically
	// operated (so that the bandwidth can change on the fly).
	sb := &switchboard{
		session:           sesh,
		switchboardConfig: config,
	}
	return sb
}

var errBrokenSwitchboard = errors.New("the switchboard is broken")

func (sb *switchboard) connsCount() int {
	// count the number of entries in conns
	var count int
	sb.conns.Range(func(_, _ interface{}) bool {
		count += 1
		return true
	})
	return count
}

func (sb *switchboard) addConn(conn net.Conn) {
	connId := atomic.AddUint32(&sb.nextConnId, 1) - 1
	sb.conns.Store(connId, conn)
	go sb.deplex(connId, conn)
}

// a pointer to connId is passed here so that the switchboard can reassign it
func (sb *switchboard) send(data []byte, connId *uint32) (n int, err error) {
	writeAndRegUsage := func(conn net.Conn, d []byte) (int, error) {
		n, err = conn.Write(d)
		if err != nil {
			sb.close("failed to write to remote " + err.Error())
			return n, err
		}
		sb.AddTx(int64(n))
		return n, nil
	}

	sb.Valve.txWait(len(data))
	connCount := sb.connsCount()
	if atomic.LoadUint32(&sb.broken) == 1 || connCount == 0 {
		return 0, errBrokenSwitchboard
	}

	if sb.strategy == UNIFORM_SPREAD {
		_, conn, err := sb.pickRandConn()
		if err != nil {
			return 0, errBrokenSwitchboard
		}
		return writeAndRegUsage(conn, data)
	} else {
		connI, ok := sb.conns.Load(*connId)
		conn := connI.(net.Conn)
		if ok {
			return writeAndRegUsage(conn, data)
		} else {
			newConnId, conn, err := sb.pickRandConn()
			if err != nil {
				return 0, errBrokenSwitchboard
			}
			connId = &newConnId
			return writeAndRegUsage(conn, data)
		}
	}

}

// returns a random connId
func (sb *switchboard) pickRandConn() (uint32, net.Conn, error) {
	connCount := sb.connsCount()
	if atomic.LoadUint32(&sb.broken) == 1 || connCount == 0 {
		return 0, nil, errBrokenSwitchboard
	}

	// there is no guarantee that sb.conns still has the same amount of entries
	// between the count loop and the pick loop
	// so if the r > len(sb.conns) at the point of range call, the last visited element is picked
	var id uint32
	var conn net.Conn
	r := rand.Intn(connCount)
	var c int
	sb.conns.Range(func(connIdI, connI interface{}) bool {
		if r == c {
			id = connIdI.(uint32)
			conn = connI.(net.Conn)
			return false
		}
		c++
		return true
	})
	return id, conn, nil
}

func (sb *switchboard) close(terminalMsg string) {
	atomic.StoreUint32(&sb.broken, 1)
	if !sb.session.IsClosed() {
		sb.session.SetTerminalMsg(terminalMsg)
		sb.session.passiveClose()
	}
}

// actively triggered by session.Close()
func (sb *switchboard) closeAll() {
	sb.conns.Range(func(key, connI interface{}) bool {
		conn := connI.(net.Conn)
		conn.Close()
		sb.conns.Delete(key)
		return true
	})
}

// deplex function costantly reads from a TCP connection
func (sb *switchboard) deplex(connId uint32, conn net.Conn) {
	buf := make([]byte, 20480)
	for {
		n, err := sb.session.UnitRead(conn, buf)
		sb.rxWait(n)
		sb.Valve.AddRx(int64(n))
		if err != nil {
			log.Debugf("a connection for session %v has closed: %v", sb.session.id, err)
			go conn.Close()
			sb.close("a connection has dropped unexpectedly")
			return
		}

		err = sb.session.recvDataFromRemote(buf[:n])
		if err != nil {
			log.Error(err)
		}
	}
}
