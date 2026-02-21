package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	frameEnd  = 0xC0
	frameEsc  = 0xDB
	transFEND = 0xDC
	transFESC = 0xDD
)

type kissFramer struct {
	// decodes a byte stream into complete KISS frames (including trailing 0xC0 delimiter)
	inFrame bool
	esc     bool
	buf     []byte
	max     int
}

func newKISSFramer(maxFrame int) *kissFramer {
	return &kissFramer{max: maxFrame}
}

// feed consumes bytes; for each complete frame found, it calls onFrame(frameBytes).
func (f *kissFramer) feed(p []byte, onFrame func([]byte)) error {
	for _, b := range p {
		if b == frameEnd {
			if f.inFrame && len(f.buf) > 0 {
				// emit raw, re-delimit with 0xC0...0xC0 for network clients
				frame := make([]byte, 0, len(f.buf)+2)
				frame = append(frame, frameEnd)
				frame = append(frame, f.buf...)
				frame = append(frame, frameEnd)
				onFrame(frame)
			}
			// reset frame
			f.inFrame = true
			f.esc = false
			f.buf = f.buf[:0]
			continue
		}
		if !f.inFrame {
			// ignore until first 0xC0
			continue
		}
		if f.esc {
			switch b {
			case transFEND:
				f.buf = append(f.buf, frameEnd)
			case transFESC:
				f.buf = append(f.buf, frameEsc)
			default:
				// unknown escape; keep as-is (conservative)
				f.buf = append(f.buf, b)
			}
			f.esc = false
		} else {
			if b == frameEsc {
				f.esc = true
			} else {
				f.buf = append(f.buf, b)
			}
		}
		if f.max > 0 && len(f.buf) > f.max {
			// reset on oversized garbage
			f.inFrame = false
			f.esc = false
			f.buf = f.buf[:0]
			return fmt.Errorf("kiss frame exceeded max (%d)", f.max)
		}
	}
	return nil
}

type client struct {
	id      uint64
	conn    net.Conn
	out     chan []byte
	closing atomic.Bool
}

type hub struct {
	dev         io.ReadWriteCloser
	listener    net.Listener
	maxFrame    int
	clientBuf   int
	idleTimeout time.Duration
	writeCh     chan []byte
	addCh       chan *client
	delCh       chan *client
	bcastCh     chan []byte
	nextID      atomic.Uint64
	dropAfter   time.Duration
}

func newHub(dev io.ReadWriteCloser, lis net.Listener, maxFrame int, clientBuf int, dropAfter time.Duration, idleTimeout time.Duration) *hub {
	return &hub{
		dev:         dev,
		listener:    lis,
		maxFrame:    maxFrame,
		clientBuf:   clientBuf,
		idleTimeout: idleTimeout,
		writeCh:     make(chan []byte, 256),
		addCh:       make(chan *client),
		delCh:       make(chan *client),
		bcastCh:     make(chan []byte, 256),
		dropAfter:   dropAfter,
	}
}

func (h *hub) run(ctx context.Context) error {
	clients := map[*client]struct{}{}

	// Device reader -> broadcast
	go func() {
		defer func() { _ = h.listener.Close() }()
		buf := make([]byte, 4096)
		fr := newKISSFramer(h.maxFrame)
		for {
			n, err := h.dev.Read(buf)
			if n > 0 {
				_ = fr.feed(buf[:n], func(frame []byte) {
					select {
					case h.bcastCh <- frame:
					case <-ctx.Done():
					}
				})
			}
			if err != nil {
				if errors.Is(err, os.ErrClosed) || errors.Is(err, net.ErrClosed) {
					return
				}
				// device read errors should stop the process; signal via closing listener
				log.Printf("device read error: %v", err)
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	// Device writer: single serialized writer
	go func() {
		for {
			select {
			case frame := <-h.writeCh:
				if len(frame) == 0 {
					continue
				}
				// frame already includes leading/trailing 0xC0
				_, err := h.dev.Write(frame)
				if err != nil {
					log.Printf("device write error: %v", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Accept loop
	go func() {
		for {
			c, err := h.listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					log.Printf("accept error: %v", err)
					continue
				}
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(30 * time.Second)
			}

			id := h.nextID.Add(1)
			cl := &client{
				id:   id,
				conn: c,
				out:  make(chan []byte, h.clientBuf),
			}
			h.addCh <- cl
		}
	}()

	for {
		select {
		case cl := <-h.addCh:
			clients[cl] = struct{}{}
			log.Printf("client %d connected from %s", cl.id, cl.conn.RemoteAddr())
			go h.clientWriter(ctx, cl)
			go h.clientReader(ctx, cl)
		case cl := <-h.delCh:
			if _, ok := clients[cl]; ok {
				delete(clients, cl)
				if cl.closing.CompareAndSwap(false, true) {
					log.Printf("client %d disconnecting addr=%s", cl.id, cl.conn.RemoteAddr())
					_ = cl.conn.Close()
					close(cl.out)
				}
				log.Printf("client %d disconnected", cl.id)
			}
		case frame := <-h.bcastCh:
			// broadcast to all clients, but never block RF path
			for cl := range clients {
				select {
				case cl.out <- frame:
				default:
					// client too slow; drop it
					log.Printf("client %d too slow; dropping", cl.id)
					h.delCh <- cl
				}
			}
		case <-ctx.Done():
			// shutdown
			for cl := range clients {
				h.delCh <- cl
			}
			return nil
		}
	}
}

func (h *hub) clientReader(ctx context.Context, cl *client) {
	defer func() { h.delCh <- cl }()
	buf := make([]byte, 4096)
	fr := newKISSFramer(h.maxFrame)
	for {
		n, err := cl.conn.Read(buf)
		if n > 0 {
			feedErr := fr.feed(buf[:n], func(frame []byte) {
				select {
				case h.writeCh <- frame:
				case <-ctx.Done():
				}
			})
			if feedErr != nil {
				log.Printf("client %d parse error: %v", cl.id, feedErr)
				return
			}
		}

		if err != nil {
			// Any read error means the client is gone (EOF/reset/etc).
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				log.Printf("client %d read timeout (idleTimeout=%s)", cl.id, h.idleTimeout)
				return
			}
			if errors.Is(err, io.EOF) {
				log.Printf("client %d read EOF (peer closed)", cl.id)
				return
			}
			log.Printf("client %d read error: %v", cl.id, err)
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (h *hub) clientWriter(ctx context.Context, cl *client) {
	defer func() { h.delCh <- cl }()
	for {
		select {
		case frame, ok := <-cl.out:
			if !ok {
				return
			}
			_ = cl.conn.SetWriteDeadline(time.Now().Add(h.dropAfter))
			_, err := cl.conn.Write(frame)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					log.Printf("client %d write timeout (dropAfter=%s)", cl.id, h.dropAfter)
				} else {
					log.Printf("client %d write error: %v", cl.id, err)
				}
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	var (
		listen      = flag.String("listen", ":8001", "TCP listen address (e.g. 127.0.0.1:8001 or :8001)")
		device      = flag.String("device", "/dev/soundmodem0", "KISS device path (e.g. /dev/soundmodem0)")
		maxFrame    = flag.Int("max-frame", 4096, "Max KISS payload bytes inside frame (safety)")
		dropAfter   = flag.Duration("drop-after", 2*time.Second, "Drop clients if writes block longer than this")
		clientBuf   = flag.Int("client-buf", 128, "Per-client outbound buffer size")
		idleTimeout = flag.Duration("idle-timeout", 0*time.Second, "Disconnect clients idle for this long (0 to disable)")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	dev, err := os.OpenFile(*device, os.O_RDWR, 0)
	if err != nil {
		log.Fatalf("open device: %v", err)
	}
	defer dev.Close()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = lis.Close() // unblocks Accept()
		_ = dev.Close() // unblocks device Read()
	}()

	log.Printf("kissmux starting: device=%s listen=%s", *device, *listen)

	h := newHub(dev, lis, *maxFrame, *clientBuf, *dropAfter, *idleTimeout)
	if err := h.run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}

	log.Printf("kissmux stopped")
}
