package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
)

type dotPipeConfig struct {
	Enabled         bool
	ReadFD, WriteFD int
}

func (c dotPipeConfig) validate() error {
	if !c.Enabled {
		if c.ReadFD != -1 || c.WriteFD != -1 {
			return errors.New("dot pipe descriptors require explicit -dot-private-pipes")
		}
		return nil
	}
	if c.ReadFD < 0 || c.WriteFD < 0 || c.ReadFD == c.WriteFD || c.ReadFD > 1<<20 || c.WriteFD > 1<<20 {
		return errors.New("dot private pipe descriptors are missing or invalid")
	}
	return nil
}

type dotPipeRuntime struct {
	store         *Store
	input, output *os.File
	once          sync.Once
	started       atomic.Bool
}

func prepareDotPrivatePipes(store *Store, config dotPipeConfig) (*dotPipeRuntime, error) {
	if e := config.validate(); e != nil {
		return nil, e
	}
	if !config.Enabled {
		return nil, nil
	}
	if store == nil {
		return nil, errDotUnavailable
	}
	if e := dotFeatureQ(store.db); e != nil {
		return nil, e
	}
	input, e := dotDuplicatePipeFD(config.ReadFD, true)
	if e != nil {
		return nil, e
	}
	output, e := dotDuplicatePipeFD(config.WriteFD, false)
	if e != nil {
		input.Close()
		return nil, e
	}
	return &dotPipeRuntime{store: store, input: input, output: output}, nil
}
func (r *dotPipeRuntime) Close() {
	if r != nil {
		r.once.Do(func() { r.input.Close(); r.output.Close() })
	}
}
func (r *dotPipeRuntime) Serve(ctx context.Context) error {
	if r == nil || ctx == nil || !r.started.CompareAndSwap(false, true) {
		return errDotUnavailable
	}
	defer r.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			r.Close()
		case <-done:
		}
	}()
	// Main's existing state lock owns this Store. Recovery cannot requeue or enable.
	if e := r.store.dotRecoverInterrupted(ctx); e != nil {
		return e
	}
	e := r.store.serveDotPrivatePipes(ctx, r.input, r.output)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if e == nil {
		return errors.New("trusted dot supervisor pipe disconnected")
	}
	return e
}
