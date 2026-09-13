package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"
)

type requestSlot struct {
	cancel context.CancelFunc
}

type inflightRequests struct {
	mu       sync.Mutex
	draining bool
	byKey    map[string]*requestSlot
	slots    map[*requestSlot]struct{}
	wg       sync.WaitGroup
}

func newInflightRequests() *inflightRequests {
	return &inflightRequests{byKey: map[string]*requestSlot{}, slots: map[*requestSlot]struct{}{}}
}

func (r *inflightRequests) start(parent context.Context, rawID json.RawMessage, run func(context.Context)) {
	ctx, cancel := context.WithCancel(parent)
	slot := &requestSlot{cancel: cancel}
	key := requestIDKey(rawID)
	r.mu.Lock()
	if r.draining {
		r.mu.Unlock()
		cancel()
		return
	}
	if key != "" {
		r.byKey[key] = slot
	}
	r.slots[slot] = struct{}{}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer r.release(key, slot)
		defer cancel()
		run(ctx)
	}()
}

func (r *inflightRequests) release(key string, slot *requestSlot) {
	r.mu.Lock()
	if r.byKey[key] == slot {
		delete(r.byKey, key)
	}
	delete(r.slots, slot)
	r.mu.Unlock()
}

func (r *inflightRequests) cancelKey(key string) bool {
	r.mu.Lock()
	slot, ok := r.byKey[key]
	r.mu.Unlock()
	if ok {
		slot.cancel()
	}
	return ok
}

func (r *inflightRequests) drain(ctx context.Context, grace, joinBound time.Duration) bool {
	r.mu.Lock()
	r.draining = true
	r.mu.Unlock()
	if r.joinWithin(ctx, grace) {
		return true
	}
	r.mu.Lock()
	for slot := range r.slots {
		slot.cancel()
	}
	r.mu.Unlock()
	return r.joinWithin(ctx, joinBound)
}

func (r *inflightRequests) joinWithin(ctx context.Context, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func requestIDKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || v == nil {
		return ""
	}
	switch id := v.(type) {
	case string:
		return strconv.Quote(id)
	case json.Number:
		return numberKey(id)
	}
	return ""
}

func numberKey(n json.Number) string {
	if i, err := n.Int64(); err == nil {
		return strconv.FormatInt(i, 10)
	}
	if u, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
		return strconv.FormatUint(u, 10)
	}
	if f, err := n.Float64(); err == nil {
		if -1<<63 <= f && f < 1<<63 {
			if i := int64(f); f == float64(i) {
				return strconv.FormatInt(i, 10)
			}
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return n.String()
}

func cancelledRequestKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := dec.Decode(&params); err != nil || len(params.RequestID) == 0 {
		return ""
	}
	return requestIDKey(params.RequestID)
}
