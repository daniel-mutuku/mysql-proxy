package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// pool caps the number of open backend connections at size.
// Callers that arrive when every connection is busy wait (up to waitTimeout)
// instead of opening a new one, so MySQL never sees more than size connections
// from the proxy, however many clients connect to the proxy.
type pool struct {
	cfg         backendConfig
	waitTimeout time.Duration

	slots chan struct{}  // one token per connection that may exist; acquiring a token = permission to hold a conn
	idle  chan *idleConn // open connections not currently lent out

	waiting atomic.Int64 // callers blocked waiting for a slot (for stats only)
	opened  atomic.Int64 // total backend connections ever dialled (for stats only)
}

type idleConn struct {
	conn  *client.Conn
	since time.Time
}

// Connections idle for longer than this are pinged before being handed out,
// in case MySQL (wait_timeout) or the network dropped them.
const pingAfterIdle = 30 * time.Second

func newPool(cfg backendConfig, size int, waitTimeout time.Duration) *pool {
	return &pool{
		cfg:         cfg,
		waitTimeout: waitTimeout,
		slots:       make(chan struct{}, size),
		idle:        make(chan *idleConn, size),
	}
}

// get returns a backend connection, reusing an idle one when possible.
// It blocks while all size connections are lent out.
func (p *pool) get(ctx context.Context) (*client.Conn, error) {
	p.waiting.Add(1)
	timer := time.NewTimer(p.waitTimeout)
	defer timer.Stop()

	select {
	case p.slots <- struct{}{}:
		p.waiting.Add(-1)
	case <-timer.C:
		p.waiting.Add(-1)
		return nil, mysql.NewError(mysql.ER_CON_COUNT_ERROR,
			fmt.Sprintf("proxy: no backend connection free within %v (pool size %d)", p.waitTimeout, cap(p.slots)))
	case <-ctx.Done():
		p.waiting.Add(-1)
		return nil, ctx.Err()
	}

	// We hold a slot. Prefer an idle connection; otherwise dial a new one.
	for {
		select {
		case ic := <-p.idle:
			if time.Since(ic.since) > pingAfterIdle && ic.conn.Ping() != nil {
				ic.conn.Close()
				continue // dead; try the next idle one or dial
			}
			return ic.conn, nil
		default:
			c, err := client.Connect(p.cfg.addr, p.cfg.user, p.cfg.pass, "")
			if err != nil {
				<-p.slots // give the slot back
				return nil, err
			}
			p.opened.Add(1)
			return c, nil
		}
	}
}

// put returns a connection to the pool. If reusable is false the connection
// is closed instead (broken, or left with session state we can't clean up).
func (p *pool) put(c *client.Conn, reusable bool) {
	if reusable {
		p.idle <- &idleConn{conn: c, since: time.Now()}
	} else {
		c.Close()
	}
	<-p.slots
}

func (p *pool) stats() (inUse, idle, waiting int, opened int64) {
	return len(p.slots), len(p.idle), int(p.waiting.Load()), p.opened.Load()
}

// close closes idle connections. Call once no more get/put calls can happen.
func (p *pool) close() {
	for {
		select {
		case ic := <-p.idle:
			ic.conn.Close()
		default:
			return
		}
	}
}
