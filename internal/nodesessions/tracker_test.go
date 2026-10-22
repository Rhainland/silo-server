package nodesessions

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

// Overlapping requests for one playback session (a HEAD probe or a second
// range request while a GET plays) each hold the session. Only the last
// release may end it, in memory and in Redis.
func TestHoldKeepsSessionUntilLastOverlappingRequestEnds(t *testing.T) {
	redisStub := startRedisStub(t)
	tracker := NewTracker(redisStub.client(t), "http://proxy-a", "proxy-a", "proxy")
	ctx := context.Background()
	info := SessionInfo{SessionID: "play-1", Type: "remux"}

	releasePlaying := tracker.Hold(ctx, info)
	const overlapping = 8
	releases := make(chan func(context.Context), overlapping)
	var wg sync.WaitGroup
	for range overlapping {
		wg.Go(func() { releases <- tracker.Hold(ctx, info) })
	}
	wg.Wait()
	close(releases)
	for release := range releases {
		wg.Go(func() {
			release(ctx)
			release(ctx) // a repeated release must not end another request's hold
		})
	}
	wg.Wait()

	if got := tracker.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount after overlapping requests ended = %d, want 1", got)
	}
	if redisStub.deleted(tracker.redisKey(info.SessionID)) {
		t.Fatal("session record deleted from Redis while a request still holds it")
	}

	releasePlaying(ctx)
	if got := tracker.ActiveCount(); got != 0 {
		t.Fatalf("ActiveCount after last request ended = %d, want 0", got)
	}
	if !redisStub.deleted(tracker.redisKey(info.SessionID)) {
		t.Fatal("session record kept in Redis after its last request ended")
	}
}

// An authoritative Remove ends the session; a hold taken before it must not
// end a hold taken after the session was registered again.
func TestHoldReleasedAfterRemoveLeavesNewHoldAlone(t *testing.T) {
	redisStub := startRedisStub(t)
	tracker := NewTracker(redisStub.client(t), "http://proxy-a", "proxy-a", "proxy")
	ctx := context.Background()
	info := SessionInfo{SessionID: "play-1", Type: "remux"}

	releaseStale := tracker.Hold(ctx, info)
	tracker.Remove(ctx, info.SessionID)
	releaseCurrent := tracker.Hold(ctx, info)
	releaseStale(ctx)

	if got := tracker.ActiveCount(); got != 1 {
		t.Fatalf("ActiveCount after stale release = %d, want 1", got)
	}
	releaseCurrent(ctx)
	if got := tracker.ActiveCount(); got != 0 {
		t.Fatalf("ActiveCount after current release = %d, want 0", got)
	}
}

// redisStub answers every command with a success reply and records the keys
// it was asked to delete. That is enough Redis for the tracker, which ignores replies
// beyond logging errors.
type redisStub struct {
	addr string
	mu   sync.Mutex
	dels map[string]int
}

func startRedisStub(t *testing.T) *redisStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stub := &redisStub{addr: ln.Addr().String(), dels: make(map[string]int)}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { stub.serve(conn) })
		}
	})
	return stub
}

func (s *redisStub) client(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: s.addr, DisableIdentity: true})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func (s *redisStub) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		args, err := readRESPCommand(r)
		if err != nil {
			return
		}
		reply := "+OK\r\n"
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			// Refusing HELLO makes the client fall back to RESP2.
			reply = "-ERR unknown command 'HELLO'\r\n"
		case "DEL":
			s.mu.Lock()
			for _, key := range args[1:] {
				s.dels[key]++
			}
			s.mu.Unlock()
			reply = ":" + strconv.Itoa(len(args)-1) + "\r\n"
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

func (s *redisStub) deleted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dels[key] > 0
}

func readRESPCommand(r *bufio.Reader) ([]string, error) {
	n, err := readRESPLength(r, '*')
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		size, err := readRESPLength(r, '$')
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

func readRESPLength(r *bufio.Reader, prefix byte) (int, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || line[0] != prefix {
		return 0, io.ErrUnexpectedEOF
	}
	return strconv.Atoi(line[1:])
}
