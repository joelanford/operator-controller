package fbc

import (
	"cmp"
	"fmt"
	"sync"
	"time"

	"go.podman.io/image/v5/docker/reference"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/operator-framework/operator-controller/internal/shared/fbc/internal/util"
)

type Node struct {
	Name           string
	VersionRelease VersionRelease
	ReleaseDate    time.Time
	ImageReference reference.Canonical
	Retracted      bool

	LifecyclePhase                 LifecyclePhase
	LifecyclePhaseEnds             *Date
	SupportedPlatformVersions      sets.Set[MajorMinor]
	RequiresUpdatePlatformVersions sets.Set[MajorMinor]

	id     int64
	idOnce sync.Once
}

func (n *Node) ID() int64 {
	n.idOnce.Do(func() {
		n.id = int64(util.HashString(n.NVR()))
	})
	return n.id
}

func (n *Node) NVR() string {
	return fmt.Sprintf("%s.v%s", n.Name, n.VR())
}

func (n *Node) VR() string {
	return n.VersionRelease.String()
}

func (n *Node) Compare(other *Node) int {
	if v := cmp.Compare(n.Name, other.Name); v != 0 {
		return v
	}
	if n.Retracted && !other.Retracted {
		return -1
	}
	if !n.Retracted && other.Retracted {
		return 1
	}
	if v := n.LifecyclePhase.Compare(other.LifecyclePhase); v != 0 {
		return v
	}
	return n.VersionRelease.Compare(other.VersionRelease)
}
