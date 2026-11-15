package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

func main() {
	addr := flag.String("addr", ":3308", "Address to listen on")
	grace := flag.Duration("grace", 10*time.Second, "How long to wait for clients to disconnect on shutdown")
	poolSize := flag.Int("pool-size", 10, "Max backend MySQL connections the proxy will open")
	poolWait := flag.Duration("pool-wait", 5*time.Second, "How long a query waits for a free backend connection before failing")
	statsEvery := flag.Duration("stats", 2*time.Second, "Log pool stats this often while busy (0 = off)")
	debug := flag.Bool("debug", false, "Log every query")
	flag.Parse()

	user := os.Getenv("PROXY_USER")
	pass := os.Getenv("PROXY_PASS")
	if user == "" || pass == "" {
		log.Fatal("PROXY_USER and PROXY_PASS environment variables must be set")
	}

	backend := backendConfig{
		addr: os.Getenv("BACKEND_ADDR"),
		user: os.Getenv("BACKEND_USER"),
		pass: os.Getenv("BACKEND_PASS"),
	}
	if backend.addr == "" || backend.user == "" {
		log.Fatal("BACKEND_ADDR and BACKEND_USER environment variables must be set")
	}

	// Cancelled on the first Ctrl+C or SIGTERM (docker stop sends SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s -> %s (pool size %d)", *addr, backend.addr, *poolSize)

	p := &proxy{
		srv:   server.NewDefaultServer(),
		user:  user,
		pass:  pass,
		pool:  newPool(backend, *poolSize, *poolWait),
		debug: *debug,
		conns: map[net.Conn]struct{}{},
	}
	if *statsEvery > 0 {
		go p.logStats(ctx, *statsEvery)
	}

	// On shutdown, closing the listener makes Accept() return an error,
	// which ends the accept loop below.
	go func() {
		<-ctx.Done()
		stop() // a second Ctrl+C now kills the process immediately
		log.Println("shutting down: no longer accepting connections")
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break // listener closed by shutdown
			}
			log.Printf("accept: %v", err)
			time.Sleep(100 * time.Millisecond) // avoid a hot loop on repeated errors
			continue
		}
		p.wg.Add(1)
		go p.handle(conn)
	}

	p.shutdown(*grace)
	p.pool.close()
}

type backendConfig struct {
	addr string
	user string
	pass string
}

type proxy struct {
	srv   *server.Server
	user  string
	pass  string
	pool  *pool
	debug bool

	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// logStats prints pool usage whenever it changes, so you can watch
// clients queue up behind a fixed number of MySQL connections.
func (p *proxy) logStats(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.mu.Lock()
		clients := len(p.conns)
		p.mu.Unlock()
		inUse, idle, waiting, opened := p.pool.stats()
		line := fmt.Sprintf("clients=%d backend: in_use=%d idle=%d waiting=%d (dialled %d total)",
			clients, inUse, idle, waiting, opened)
		if line != last {
			log.Print(line)
			last = line
		}
	}
}

func (p *proxy) track(conn net.Conn) {
	p.mu.Lock()
	p.conns[conn] = struct{}{}
	p.mu.Unlock()
}

func (p *proxy) untrack(conn net.Conn) {
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
}

// shutdown waits for clients to leave on their own, then force-closes the rest.
func (p *proxy) shutdown(grace time.Duration) {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	p.mu.Lock()
	active := len(p.conns)
	p.mu.Unlock()
	log.Printf("waiting up to %v for %d client(s) to disconnect", grace, active)

	select {
	case <-done:
		log.Println("all clients disconnected")
	case <-time.After(grace):
		p.mu.Lock()
		log.Printf("grace period over: closing %d client(s)", len(p.conns))
		for c := range p.conns {
			c.Close() // unblocks HandleCommand, so each handler returns
		}
		p.mu.Unlock()
		<-done
	}
	log.Println("shutdown complete")
}

func (p *proxy) handle(conn net.Conn) {
	defer p.wg.Done()
	p.track(conn)
	defer p.untrack(conn)
	defer conn.Close()

	addr := conn.RemoteAddr().String()

	// No backend is opened here: the session borrows one from the pool per
	// command. That is what keeps MySQL connections bounded by -pool-size.
	sess := &session{pool: p.pool, addr: addr, debug: p.debug}
	defer sess.close()

	c, err := p.srv.NewConn(conn, p.user, p.pass, sess)
	if err != nil {
		log.Printf("%s: handshake failed: %v", addr, err)
		return
	}
	if p.debug {
		log.Printf("%s: client connected", addr)
	}

	for !c.Closed() {
		if err := c.HandleCommand(); err != nil {
			// ErrBadConn here almost always means the client hung up without
			// sending COM_QUIT (PHP does this at the end of a request): normal.
			if p.debug || !errors.Is(err, mysql.ErrBadConn) {
				log.Printf("%s: %v", addr, err)
			}
			return
		}
	}
	if p.debug {
		log.Printf("%s: client disconnected", addr)
	}
}
