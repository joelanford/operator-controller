package fbc

import (
	"github.com/blang/semver/v4"
)

type NodePredicate func(*Graph, *Node) bool

func AllNodes() NodePredicate {
	return func(_ *Graph, _ *Node) bool {
		return true
	}
}

func PackageNodes(pkgName string) NodePredicate {
	return func(g *Graph, node *Node) bool {
		return node.Name == pkgName
	}
}

func NodeInRange(rng semver.Range) NodePredicate {
	return func(_ *Graph, actual *Node) bool {
		return rng(actual.VersionRelease.Version)
	}
}

func ExactVersionRelease(vr VersionRelease) NodePredicate {
	return func(g *Graph, actual *Node) bool {
		return actual.VersionRelease.Compare(vr) == 0
	}
}

func IsNode(expect *Node) NodePredicate {
	if expect == nil {
		return func(_ *Graph, _ *Node) bool { return false }
	}
	return func(_ *Graph, actual *Node) bool {
		return expect == actual
	}
}

func SuccessorOf(from *Node) NodePredicate {
	if from == nil {
		return func(_ *Graph, _ *Node) bool { return false }
	}
	return func(g *Graph, node *Node) bool {
		for successor := range g.From(from) {
			if successor == node {
				return true
			}
		}
		return false
	}
}

type EdgePredicate func(*Graph, *Node, *Node, float64) bool

func AllEdges() EdgePredicate {
	return func(*Graph, *Node, *Node, float64) bool { return true }
}

func AndNodes(ps ...NodePredicate) NodePredicate {
	return func(graph *Graph, node *Node) bool {
		for _, p := range ps {
			if !p(graph, node) {
				return false
			}
		}
		return true
	}
}

func OrNodes(ps ...NodePredicate) NodePredicate {
	return func(graph *Graph, node *Node) bool {
		for _, p := range ps {
			if p(graph, node) {
				return true
			}
		}
		return false
	}
}
