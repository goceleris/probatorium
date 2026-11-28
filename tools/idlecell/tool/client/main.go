// Command idleclient opens N keep-alive HTTP/1.1 connections to the server and
// leaves them idle (celeris#1430 phase 1; scratch tool, not in the repository).
// Each connection sends one "GET /ping" (header X-Hold: 1), reads the 2-byte
// body, then stays open with a goroutine blocked in Read. It exits when every
// connection has been closed by the server (or after -max). Source addresses
// rotate over 127.0.0.2..127.0.0.9 so that 50k connections do not run out of
// ephemeral ports. Prints OPENED and CLOSED lines with unix-ms stamps.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "server address")
	n := flag.Int("n", 1000, "connections")
	par := flag.Int("par", 32, "concurrent dials")
	max := flag.Duration("max", 4*time.Minute, "give up after")
	probeGap := flag.Duration("probe-gap", 0, "first-request probe: idle gap before each probe request (0: no probes)")
	probeCount := flag.Int("probe-count", 8, "first-request probe: requests per kind (new connection, held connection)")
	probeAfter := flag.Duration("probe-after", 13*time.Second, "first-request probe: wait after the connections are open before the first gap")
	flag.Parse()
	if *n == 0 {
		return
	}
	// wait for the server to listen
	for dl := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		c, err := net.Dial("tcp", *addr)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(dl) {
			fmt.Println("idleclient: server never listened")
			os.Exit(2)
		}
	}
	reserved := make([]net.Conn, *probeCount)
	req := []byte("GET /ping HTTP/1.1\r\nHost: x\r\nX-Hold: 1\r\n\r\n")
	var opened, failed, closed atomic.Int64
	var firstClose atomic.Int64
	var wg, readers sync.WaitGroup
	sem := make(chan struct{}, *par)
	t0 := time.Now()
	for i := 0; i < *n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			var lastErr error
			for try := 0; try < 5; try++ {
				d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, byte(2+i%8))}, Timeout: 10 * time.Second}
				c, err := d.Dial("tcp", *addr)
				if err != nil {
					lastErr = err
					time.Sleep(100 * time.Millisecond)
					continue
				}
				c.SetDeadline(time.Now().Add(20 * time.Second))
				if _, err := c.Write(req); err != nil {
					c.Close()
					lastErr = err
					continue
				}
				buf := make([]byte, 256)
				got := 0
				for !strings.HasSuffix(string(buf[:got]), "ok") && got < len(buf) {
					m, err := c.Read(buf[got:])
					got += m
					if err != nil {
						lastErr = err
						break
					}
				}
				if !strings.HasSuffix(string(buf[:got]), "ok") || !strings.HasPrefix(string(buf[:got]), "HTTP/1.1 200") {
					c.Close()
					continue
				}
				c.SetDeadline(time.Time{})
				opened.Add(1)
				if *probeGap > 0 && i < *probeCount {
					reserved[i] = c // kept for a probe: no reader goroutine on it
					return
				}
				readers.Add(1)
				go func() {
					defer readers.Done()
					b := make([]byte, 1)
					_, err := c.Read(b)
					if err != nil || true {
						now := time.Now().UnixMilli()
						firstClose.CompareAndSwap(0, now)
						closed.Add(1)
						_ = err
					}
					c.Close()
				}()
				return
			}
			failed.Add(1)
			if lastErr != nil && failed.Load() < 5 {
				fmt.Println("idleclient: conn failed:", lastErr)
			}
		}(i)
	}
	wg.Wait()
	fmt.Printf("OPENED n=%d opened=%d failed=%d in=%v unix_ms=%d\n", *n, opened.Load(), failed.Load(), time.Since(t0).Round(time.Millisecond), time.Now().UnixMilli())
	if *probeGap > 0 {
		time.Sleep(*probeAfter)
		var newLat, heldLat []time.Duration
		for k := 0; k < *probeCount; k++ {
			time.Sleep(*probeGap)
			// a new connection: dial, send, time to the whole response
			t0 := time.Now()
			c, err := net.Dial("tcp", *addr)
			if err == nil {
				_, err = c.Write(req)
				buf := make([]byte, 256)
				got := 0
				for err == nil && !strings.HasSuffix(string(buf[:got]), "ok") && got < len(buf) {
					var m int
					m, err = c.Read(buf[got:])
					got += m
				}
				c.Close()
			}
			if err != nil {
				fmt.Println("PROBE new error:", err)
			} else {
				newLat = append(newLat, time.Since(t0))
			}
			time.Sleep(*probeGap)
			// a held keep-alive connection, idle since its first request: send, time to the whole response
			hc := reserved[k]
			if hc == nil {
				continue
			}
			t1 := time.Now()
			_, err = hc.Write(req)
			buf := make([]byte, 256)
			got := 0
			for err == nil && !strings.HasSuffix(string(buf[:got]), "ok") && got < len(buf) {
				var m int
				m, err = hc.Read(buf[got:])
				got += m
			}
			if err != nil {
				fmt.Println("PROBE held error:", err)
			} else {
				heldLat = append(heldLat, time.Since(t1))
			}
		}
		for _, kv := range []struct {
			name string
			l    []time.Duration
		}{{"new", newLat}, {"held", heldLat}} {
			if len(kv.l) == 0 {
				continue
			}
			sort.Slice(kv.l, func(a, b int) bool { return kv.l[a] < kv.l[b] })
			fmt.Printf("PROBE %s gap=%v n=%d min=%v median=%v max=%v all_us=%v\n", kv.name, *probeGap, len(kv.l),
				kv.l[0].Round(10*time.Microsecond), kv.l[len(kv.l)/2].Round(10*time.Microsecond), kv.l[len(kv.l)-1].Round(10*time.Microsecond), us(kv.l))
		}
	}
	done := make(chan struct{})
	go func() { readers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(*max):
		fmt.Println("idleclient: max wait reached")
	}
	fmt.Printf("CLOSED closed=%d first_close_unix_ms=%d last_unix_ms=%d\n", closed.Load(), firstClose.Load(), time.Now().UnixMilli())
}

func us(l []time.Duration) []int64 {
	o := make([]int64, len(l))
	for i, d := range l {
		o[i] = d.Microseconds()
	}
	return o
}
