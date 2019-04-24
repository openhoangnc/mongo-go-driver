package topology

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/x/network/address"
)

func TestPool(t *testing.T) {
	t.Run("newPool", func(t *testing.T) {
		t.Run("should be connected", func(t *testing.T) {
			p := newPool(address.Address(""), 2)
			err := p.connect()
			noerr(t, err)
			if p.connected != connected {
				t.Errorf("Expected new pool to be connected. got %v; want %v", p.connected, connected)
			}
		})
	})
	t.Run("close", func(t *testing.T) {
		t.Run("can't put connection from different pool", func(t *testing.T) {
			p1 := newPool(address.Address(""), 2)
			p2 := newPool(address.Address(""), 2)
			err := p1.connect()
			noerr(t, err)
			err = p2.connect()
			noerr(t, err)

			c1 := &connection{pool: p1}
			want := ErrWrongPool
			got := p2.close(c1)
			if got != want {
				t.Errorf("Errors do not match. got %v; want %v", got, want)
			}
		})
	})
	t.Run("put", func(t *testing.T) {
		t.Run("can't put connection from different pool", func(t *testing.T) {
			p1 := newPool(address.Address(""), 2)
			p2 := newPool(address.Address(""), 2)
			err := p1.connect()
			noerr(t, err)
			err = p2.connect()
			noerr(t, err)

			c1 := &connection{pool: p1}
			want := ErrWrongPool
			got := p2.put(c1)
			if got != want {
				t.Errorf("Errors do not match. got %v; want %v", got, want)
			}
		})
	})
	t.Run("Disconnect", func(t *testing.T) {
		t.Run("cannot disconnect twice", func(t *testing.T) {
			p := newPool(address.Address(""), 2)
			err := p.connect()
			noerr(t, err)
			err = p.disconnect(context.Background())
			noerr(t, err)
			err = p.disconnect(context.Background())
			if err != ErrPoolDisconnected {
				t.Errorf("Should not be able to call disconnect twice. got %v; want %v", err, ErrPoolDisconnected)
			}
		})
		t.Run("closes idle connections", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			conns := [3]*connection{}
			for idx := range [3]struct{}{} {
				conns[idx], err = p.get(context.Background())
				noerr(t, err)
			}
			for idx := range [3]struct{}{} {
				err = p.put(conns[idx])
				noerr(t, err)
			}
			if d.lenopened() != 3 {
				t.Errorf("Should have opened 3 connections, but didn't. got %d; want %d", d.lenopened(), 3)
			}
			err = p.disconnect(context.Background())
			noerr(t, err)
			if d.lenclosed() != 3 {
				t.Errorf("Should have closed 3 connections, but didn't. got %d; want %d", d.lenclosed(), 3)
			}
			close(cleanup)
		})
		t.Run("closes inflight connections when context expires", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			conns := [3]*connection{}
			for idx := range [3]struct{}{} {
				conns[idx], err = p.get(context.Background())
				noerr(t, err)
			}
			for idx := range [2]struct{}{} {
				err = p.put(conns[idx])
				noerr(t, err)
			}
			if d.lenopened() != 3 {
				t.Errorf("Should have opened 3 connections, but didn't. got %d; want %d", d.lenopened(), 3)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Microsecond)
			cancel()
			err = p.disconnect(ctx)
			noerr(t, err)
			if d.lenclosed() != 3 {
				t.Errorf("Should have closed 3 connections, but didn't. got %d; want %d", d.lenclosed(), 3)
			}
			close(cleanup)
			err = p.close(conns[2])
			noerr(t, err)
		})
		t.Run("properly sets the connection state on return", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			c, err := p.get(context.Background())
			noerr(t, err)
			err = p.close(c)
			noerr(t, err)
			if d.lenopened() != 1 {
				t.Errorf("Should have opened 1 connections, but didn't. got %d; want %d", d.lenopened(), 1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Microsecond)
			defer cancel()
			err = p.disconnect(ctx)
			noerr(t, err)
			if d.lenclosed() != 1 {
				t.Errorf("Should have closed 1 connections, but didn't. got %d; want %d", d.lenclosed(), 1)
			}
			close(cleanup)
			state := atomic.LoadInt32(&p.connected)
			if state != disconnected {
				t.Errorf("Should have set the connection state on return. got %d; want %d", state, disconnected)
			}
		})
	})
	t.Run("Connect", func(t *testing.T) {
		t.Run("can reconnect a disconnected pool", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			c, err := p.get(context.Background())
			noerr(t, err)
			gen := c.generation
			if gen != 1 {
				t.Errorf("Connection should have a newer generation. got %d; want %d", gen, 1)
			}
			err = p.put(c)
			noerr(t, err)
			if d.lenopened() != 1 {
				t.Errorf("Should have opened 1 connections, but didn't. got %d; want %d", d.lenopened(), 1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = p.disconnect(ctx)
			noerr(t, err)
			if d.lenclosed() != 1 {
				t.Errorf("Should have closed 1 connections, but didn't. got %d; want %d", d.lenclosed(), 1)
			}
			close(cleanup)
			state := atomic.LoadInt32(&p.connected)
			if state != disconnected {
				t.Errorf("Should have set the connection state on return. got %d; want %d", state, disconnected)
			}
			err = p.connect()
			noerr(t, err)

			c, err = p.get(context.Background())
			noerr(t, err)
			gen = atomic.LoadUint64(&c.generation)
			if gen != 2 {
				t.Errorf("Connection should have a newer generation. got %d; want %d", gen, 2)
			}
			err = p.put(c)
			noerr(t, err)
			if d.lenopened() != 2 {
				t.Errorf("Should have opened 3 connections, but didn't. got %d; want %d", d.lenopened(), 2)
			}
		})
		t.Run("cannot connect multiple times without disconnect", func(t *testing.T) {
			p := newPool(address.Address(""), 3)
			err := p.connect()
			noerr(t, err)
			err = p.connect()
			if err != ErrPoolConnected {
				t.Errorf("Shouldn't be able to connect to already connected pool. got %v; want %v", err, ErrPoolConnected)
			}
			err = p.connect()
			if err != ErrPoolConnected {
				t.Errorf("Shouldn't be able to connect to already connected pool. got %v; want %v", err, ErrPoolConnected)
			}
			err = p.disconnect(context.Background())
			noerr(t, err)
			err = p.connect()
			if err != nil {
				t.Errorf("Should be able to connect to pool after disconnect. got %v; want <nil>", err)
			}
		})
		t.Run("can disconnect and reconnect multiple times", func(t *testing.T) {
			p := newPool(address.Address(""), 3)
			err := p.connect()
			noerr(t, err)
			err = p.disconnect(context.Background())
			noerr(t, err)
			err = p.connect()
			if err != nil {
				t.Errorf("Should be able to connect to disconnected pool. got %v; want <nil>", err)
			}
			err = p.disconnect(context.Background())
			noerr(t, err)
			err = p.connect()
			if err != nil {
				t.Errorf("Should be able to connect to disconnected pool. got %v; want <nil>", err)
			}
			err = p.disconnect(context.Background())
			noerr(t, err)
			err = p.connect()
			if err != nil {
				t.Errorf("Should be able to connect to pool after disconnect. got %v; want <nil>", err)
			}
		})
	})
	t.Run("Get", func(t *testing.T) {
		t.Run("return context error when already cancelled", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			cancel()
			_, err = p.get(ctx)
			if err != context.Canceled {
				t.Errorf("Should return context error when already cancelled. got %v; want %v", err, context.Canceled)
			}
			close(cleanup)
		})
		t.Run("return error when attempting to create new connection", func(t *testing.T) {
			want := errors.New("create new connection error")
			var dialer DialerFunc = func(context.Context, string, string) (net.Conn, error) { return nil, want }
			p := newPool(address.Address(""), 2, WithDialer(func(Dialer) Dialer { return dialer }))
			err := p.connect()
			noerr(t, err)
			_, got := p.get(context.Background())
			if got != want {
				t.Errorf("Should return error from calling New. got %v; want %v", got, want)
			}
		})
		t.Run("adds connection to inflight pool", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 1, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := p.get(ctx)
			noerr(t, err)
			inflight := len(p.opened)
			if inflight != 1 {
				t.Errorf("Incorrect number of inlight connections. got %d; want %d", inflight, 1)
			}
			err = p.close(c)
			noerr(t, err)
			close(cleanup)
		})
		t.Run("closes expired connections", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 2, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(
				address.Address(addr.String()), 3,
				WithDialer(func(Dialer) Dialer { return d }),
				WithIdleTimeout(func(time.Duration) time.Duration { return 10 * time.Millisecond }),
			)
			err := p.connect()
			noerr(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := p.get(ctx)
			noerr(t, err)
			if d.lenopened() != 1 {
				t.Errorf("Should have opened 1 connection, but didn't. got %d; want %d", d.lenopened(), 1)
			}
			err = p.put(c)
			noerr(t, err)
			time.Sleep(15 * time.Millisecond)
			if d.lenclosed() != 0 {
				t.Errorf("Should have closed 0 connections, but didn't. got %d; want %d", d.lenopened(), 0)
			}
			c, err = p.get(ctx)
			noerr(t, err)
			if d.lenopened() != 2 {
				t.Errorf("Should have opened 2 connections, but didn't. got %d; want %d", d.lenopened(), 2)
			}
			time.Sleep(10 * time.Millisecond)
			if d.lenclosed() != 1 {
				t.Errorf("Should have closed 1 connection, but didn't. got %d; want %d", d.lenopened(), 1)
			}
			close(cleanup)
		})
		t.Run("recycles connections", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			for range [3]struct{}{} {
				c, err := p.get(context.Background())
				noerr(t, err)
				err = p.put(c)
				noerr(t, err)
				if d.lenopened() != 1 {
					t.Errorf("Should have opened 1 connection, but didn't. got %d; want %d", d.lenopened(), 1)
				}
			}
			close(cleanup)
		})
		t.Run("cannot get from disconnected pool", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 3, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Microsecond)
			defer cancel()
			err = p.disconnect(ctx)
			noerr(t, err)
			_, err = p.get(context.Background())
			if err != ErrPoolDisconnected {
				t.Errorf("Should get error from disconnected pool. got %v; want %v", err, ErrPoolDisconnected)
			}
			close(cleanup)
		})
		t.Run("pool closes excess connections when returned", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 1, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			conns := [3]*connection{}
			for idx := range [3]struct{}{} {
				conns[idx], err = p.get(context.Background())
				noerr(t, err)
			}
			for idx := range [3]struct{}{} {
				err = p.put(conns[idx])
				noerr(t, err)
			}
			if d.lenopened() != 3 {
				t.Errorf("Should have opened 3 connections, but didn't. got %d; want %d", d.lenopened(), 3)
			}
			if d.lenclosed() != 2 {
				t.Errorf("Should have closed 2 connections, but didn't. got %d; want %d", d.lenclosed(), 2)
			}
			close(cleanup)
		})
		t.Run("Cannot starve connection request", func(t *testing.T) {
			cleanup := make(chan struct{})
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 1, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			conn, err := p.get(context.Background())
			if d.lenopened() != 1 {
				t.Errorf("Should have opened 1 connections, but didn't. got %d; want %d", d.lenopened(), 1)
			}

			var wg sync.WaitGroup

			wg.Add(1)
			ch := make(chan struct{})
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				ch <- struct{}{}
				_, err := p.get(ctx)
				if err != nil {
					t.Errorf("Should not be able to starve connection request, but got error: %v", err)
				}
				wg.Done()
			}()
			<-ch
			runtime.Gosched()
			err = p.put(conn)
			noerr(t, err)
			wg.Wait()
			close(cleanup)
		})
	})
	t.Run("Connection", func(t *testing.T) {
		t.Run("Connection Close Does Not Error After Pool Is Disconnected", func(t *testing.T) {
			cleanup := make(chan struct{})
			defer close(cleanup)
			addr := bootstrapConnections(t, 3, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 4, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			c, err := p.get(context.Background())
			noerr(t, err)
			c1 := &Connection{connection: c}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err = p.disconnect(ctx)
			noerr(t, err)
			err = c1.Close()
			if err != nil {
				t.Errorf("Connection Close should not error after Pool is Disconnected, but got error: %v", err)
			}
		})
		t.Run("Does not return to pool twice", func(t *testing.T) {
			cleanup := make(chan struct{})
			defer close(cleanup)
			addr := bootstrapConnections(t, 1, func(nc net.Conn) {
				<-cleanup
				nc.Close()
			})
			d := newdialer(&net.Dialer{})
			p := newPool(address.Address(addr.String()), 4, WithDialer(func(Dialer) Dialer { return d }))
			err := p.connect()
			noerr(t, err)
			c, err := p.get(context.Background())
			c1 := &Connection{connection: c}
			noerr(t, err)
			if len(p.conns) != 0 {
				t.Errorf("Should be no connections in pool. got %d; want %d", len(p.conns), 0)
			}
			err = c1.Close()
			noerr(t, err)
			err = c1.Close()
			noerr(t, err)
			if len(p.conns) != 1 {
				t.Errorf("Should not return connection to pool twice. got %d; want %d", len(p.conns), 1)
			}
		})
	})
}