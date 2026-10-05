// Package relay moves WebSocket messages between two connections without
// interpreting them. It is used for all three CDP hops:
//
//	client <-> coordinator <-> worker <-> browser
//
// Message type and boundaries are preserved. Nothing is buffered beyond a
// single frame, so a stalled peer cannot make the process grow without bound.
package relay

import (
	"io"
	"time"

	"github.com/gorilla/websocket"
)

// MaxMessageBytes bounds a single CDP frame. CDP is small JSON; large values
// usually mean a misbehaving peer. A var so tests can lower it.
var MaxMessageBytes int64 = 32 << 20 // 32 MiB

// WriteTimeout bounds a single write. A var so tests can lower it.
var WriteTimeout = 60 * time.Second

// Pipe copies messages in both directions until either side closes or errors.
// It always closes both connections before returning.
func Pipe(a, b *websocket.Conn) (err error) {
	a.SetReadLimit(MaxMessageBytes)
	b.SetReadLimit(MaxMessageBytes)

	done := make(chan error, 2)
	go copyMessages(a, b, done)
	go copyMessages(b, a, done)
	err = <-done

	// Unblock the surviving goroutine.
	a.Close()
	b.Close()
	<-done
	return err
}

func copyMessages(dst, src *websocket.Conn, done chan<- error) {
	for {
		mt, r, err := src.NextReader()
		if err != nil {
			done <- err
			return
		}
		w, err := dst.NextWriter(mt)
		if err != nil {
			done <- err
			return
		}
		_ = dst.SetWriteDeadline(time.Now().Add(WriteTimeout))
		if _, err := io.Copy(w, r); err != nil {
			done <- err
			return
		}
		if err := w.Close(); err != nil {
			done <- err
			return
		}
	}
}
