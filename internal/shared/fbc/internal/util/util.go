package util

import (
	"hash/fnv"
	"iter"
	"maps"
	"slices"
)

func KeySlice[K comparable, IV, OV any](s []IV, kv func(IV) (K, OV)) map[K]OV {
	m := make(map[K]OV, len(s))
	for _, iv := range s {
		k, ov := kv(iv)
		m[k] = ov
	}
	return m
}

func MapSlice[I, O any](in []I, key func(I) O) []O {
	out := make([]O, len(in))
	for i := range in {
		out[i] = key(in[i])
	}
	return out
}

func OrderedMap[K comparable, V any](m map[K]V, cmp func(a, b K) int) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		orderedKeys := slices.SortedFunc(maps.Keys(m), cmp)
		for _, k := range orderedKeys {
			v := m[k]
			if !yield(k, v) {
				return
			}
		}
	}
}

type Comparer[T any] interface {
	Compare(T) int
}

func Compare[T Comparer[T]](a, b T) int {
	return a.Compare(b)
}

func HashString(s string) uint64 {
	h := fnv.New64a()
	if _, err := h.Write([]byte(s)); err != nil {
		panic(err)
	}
	return h.Sum64()
}
