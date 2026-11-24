package e2e

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type tcpProxy struct {
	address  string
	target   string
	mu       sync.Mutex
	listener net.Listener
	online   bool
	gen      uint64
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	p := &tcpProxy{
		address: freeAddress(t),
		target:  target,
		conns:   make(map[net.Conn]struct{}),
	}
	if err := p.Resume(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.Drop()
		p.wg.Wait()
	})
	return p
}

func (p *tcpProxy) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.online {
		return nil
	}
	listener, err := net.Listen("tcp", p.address)
	if err != nil {
		return err
	}
	p.gen++
	gen := p.gen
	p.listener = listener
	p.online = true
	p.wg.Add(1)
	go p.acceptLoop(listener, gen)
	return nil
}

func (p *tcpProxy) Drop() error {
	p.mu.Lock()
	if !p.online {
		p.mu.Unlock()
		return nil
	}
	p.gen++
	p.online = false
	listener := p.listener
	connections := make([]net.Conn, 0, len(p.conns))
	for conn := range p.conns {
		connections = append(connections, conn)
	}
	p.mu.Unlock()
	var err error
	if listener != nil {
		err = listener.Close()
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	return err
}

func (p *tcpProxy) acceptLoop(listener net.Listener, gen uint64) {
	defer p.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go p.handle(conn, gen)
	}
}

func (p *tcpProxy) handle(clientConn net.Conn, gen uint64) {
	defer p.wg.Done()
	upstream, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		_ = clientConn.Close()
		return
	}
	if !p.registerPair(clientConn, upstream, gen) {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}
	defer p.unregisterPair(clientConn, upstream)
	copyDone := make(chan struct{}, 2)
	go proxyCopy(clientConn, upstream, copyDone)
	go proxyCopy(upstream, clientConn, copyDone)
	<-copyDone
	_ = clientConn.Close()
	_ = upstream.Close()
	<-copyDone
}

func (p *tcpProxy) registerPair(clientConn, upstream net.Conn, gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.online || p.gen != gen {
		return false
	}
	p.conns[clientConn] = struct{}{}
	p.conns[upstream] = struct{}{}
	return true
}

func (p *tcpProxy) unregisterPair(clientConn, upstream net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, clientConn)
	delete(p.conns, upstream)
}

func proxyCopy(destination, source net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(destination, source)
	done <- struct{}{}
}
