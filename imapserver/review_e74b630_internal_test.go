package imapserver

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// Force context's public AfterFunc propagation path so retained child
// cancellation registrations can be counted without heap heuristics.
type e74TrackedContext struct {
	context.Context
	retained atomic.Int32
}

func (*e74TrackedContext) Value(any) any { return nil }
func (p *e74TrackedContext) AfterFunc(func()) func() bool {
	p.retained.Add(1)
	var once sync.Once
	return func() bool {
		stopped := false
		once.Do(func() { p.retained.Add(-1); stopped = true })
		return stopped
	}
}

type e74RejectListener struct{ connection net.Conn }

func (l *e74RejectListener) Accept() (net.Conn, error) {
	if l.connection == nil {
		return nil, net.ErrClosed
	}
	c := l.connection
	l.connection = nil
	return c, nil
}
func (*e74RejectListener) Close() error   { return nil }
func (*e74RejectListener) Addr() net.Addr { return nil }

func TestReviewE74RejectedConnectionsReleaseContext(t *testing.T) {
	for _, serve := range []bool{false, true} {
		t.Run(map[bool]string{false: "ServeConn", true: "Serve"}[serve], func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			tracked := &e74TrackedContext{Context: parent}
			s := New(nil, &Options{Limits: Limits{MaxConnections: 1}})
			active, peer := net.Pipe()
			defer active.Close()
			defer peer.Close()
			c, _ := newConn(context.Background(), s, active)
			defer c.cancel(nil)
			if err := s.register(c); err != nil {
				t.Fatal(err)
			}
			defer s.unregister(c)
			for i := 0; i < 50; i++ {
				rejected, client := net.Pipe()
				if serve {
					if err := s.Serve(tracked, &e74RejectListener{rejected}, nil); err != nil {
						t.Fatal(err)
					}
				} else if err := s.ServeConn(tracked, rejected, nil); !errors.Is(err, errConnectionLimit) {
					t.Fatal(err)
				}
				_ = client.Close()
				if retained := tracked.retained.Load(); retained != 0 {
					t.Fatalf("rejection %d retained %d cancellation callbacks", i, retained)
				}
			}
		})
	}
}
