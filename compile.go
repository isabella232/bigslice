// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package bigslice

import (
	"fmt"
	"strings"
)

// Pipeline returns the sequence of slices that may be pipelined
// starting from slice. Slices that do not have shuffle dependencies
// may be pipelined together.
func pipeline(slice Slice) (slices []Slice) {
	for {
		slices = append(slices, slice)
		if slice.NumDep() != 1 {
			return
		}
		dep := slice.Dep(0)
		if dep.Shuffle {
			return
		}
		slice = dep.Slice
	}
}

// Compile compiles the provided slice into a set of task graphs,
// each representing the computation for one shard of the slice. The
// slice is produced by the provided invocation. Compile coalesces
// slice operations that can be pipelined into single tasks, creating
// wide dependencies only at shuffle boundaries. The provided namer
// must mint names that are unique to the session. The order in which
// the namer is invoked is guaranteed to be deterministic.
//
// TODO(marius): we don't currently reuse tasks across compilations,
// even though this could sometimes safely be done (when the number
// of partitions and the kind of partitioner matches at shuffle
// boundaries). We should at least support this use case to avoid
// redundant computations.
//
// TODO(marius): an alternative model for propagating invocations is
// to provide each actual invocation with a "root" slice from where
// all other slices must be derived. This simplifies the
// implementation but may make the API a little confusing.
func compile(namer taskNamer, inv Invocation, slice Slice) ([]*Task, error) {
	// Pipeline slices and create a task for each underlying shard,
	// pipelining the eligible computations.
	tasks := make([]*Task, slice.NumShard())
	slices := pipeline(slice)
	var ops []string
	for i := len(slices) - 1; i >= 0; i-- {
		ops = append(ops, slices[i].Op())
	}
	name := namer.New(strings.Join(ops, "_"))
	for i := range tasks {
		tasks[i] = &Task{
			Type:         slices[0],
			Name:         fmt.Sprintf("%s@%d:%d", name, len(tasks), i),
			Invocation:   inv,
			NumPartition: 1,
		}
	}
	// Pipeline execution, folding multiple frame operations
	// into a single task by composing their readers.
	for i := len(slices) - 1; i >= 0; i-- {
		for shard := range tasks {
			var (
				shard  = shard
				reader = slices[i].Reader
				prev   = tasks[shard].Do
			)
			if prev == nil {
				// First frame reads the input directly.
				tasks[shard].Do = func(readers []Reader) Reader {
					return reader(shard, readers)
				}
			} else {
				// Subsequent frames read the previous frame's output.
				tasks[shard].Do = func(readers []Reader) Reader {
					return reader(shard, []Reader{prev(readers)})
				}
			}
		}
	}
	// Now capture the dependencies; they are encoded in the last slice.
	lastSlice := slices[len(slices)-1]
	for i := 0; i < lastSlice.NumDep(); i++ {
		dep := lastSlice.Dep(i)
		deptasks, err := compile(namer, inv, dep)
		if err != nil {
			return nil, err
		}
		if !dep.Shuffle {
			panic("non-pipelined non-shuffle dependency")
		}
		// Assign a partitioner and partition width our dependencies, so that
		// these are properly partitioned at the time of computation.
		for _, task := range deptasks {
			task.NumPartition = slice.NumShard()
			task.Hasher = lastSlice.Hasher()
		}
		// Each shard reads different partitions from all of the previous tasks's shards.
		for partition := range tasks {
			tasks[partition].Deps = append(tasks[partition].Deps, TaskDep{deptasks, partition})
		}
	}
	return tasks, nil
}

type taskNamer map[string]int

func (n taskNamer) New(name string) string {
	c := n[name]
	n[name]++
	if c == 0 {
		return name
	}
	return fmt.Sprintf("%s%d", name, c)
}
