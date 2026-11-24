package e2e

import (
	"io"
	"net"
	"sync"
	"testing"
)

type tcpProxy struct {
	address  string
	target   string
	mu       sync.Mutex
	listener net.Listener
	online   bool
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
	p.listener = listener
	p.online = true
	p.wg.Add(1)
	go p.acceptLoop(listener)
	return nil
}

func (p *tcpProxy) Drop() error {
	p.mu.Lock()
	if !p.online {
		p.mu.Unlock()
		return nil
	}
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

func (p *tcpProxy) acceptLoop(listener net.Listener) {
	defer p.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go p.handle(conn)
	}
}

func (p *tcpProxy) handle(client net.Conn) {
	defer p.wg.Done()
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	if !p.registerPair(client, upstream) {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	defer p.unregisterPair(client, upstream)
	copyDone := make(chan struct{}, 2)
	go proxyCopy(client, upstream, copyDone)
	go proxyCopy(upstream, client, copyDone)
	<-copyDone
	_ = client.Close()
	_ = upstream.Close()
	<-copyDone
}

func (p *tcpProxy) registerPair(client, upstream net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.online {
		return false
	}
	p.conns[client] = struct{}{}
	p.conns[upstream] = struct{}{}
	return true
}

func (p *tcpProxy) unregisterPair(client, upstream net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, client)
	delete(p.conns, upstream)
}

func proxyCopy(destination, source net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(destination, source)
	done <- struct{}{}
}
