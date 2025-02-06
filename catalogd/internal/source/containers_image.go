package source

import (
	"context"
	"fmt"
	"io/fs"
	"time"

	"github.com/containers/image/v5/docker/reference"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	catalogdv1 "github.com/operator-framework/operator-controller/catalogd/api/v1"
	imageutil "github.com/operator-framework/operator-controller/internal/util/image"
)

type ContainersImageRegistry struct {
	Cache  imageutil.Cache
	Puller imageutil.Puller
}

func (i *ContainersImageRegistry) Unpack(ctx context.Context, id string, ref string) (*Result, error) {
	fsys, canonicalRef, modTime, err := i.Puller.Pull(ctx, id, ref, i.Cache)
	if err != nil {
		return nil, err
	}

	return successResult(fsys, canonicalRef, modTime)
}

func successResult(fsys fs.FS, ref reference.Canonical, modTime time.Time) (*Result, error) {
	return &Result{
		FS: fsys,
		ResolvedSource: &catalogdv1.ResolvedCatalogSource{
			Type: catalogdv1.SourceTypeImage,
			Image: &catalogdv1.ResolvedImageSource{
				Ref: ref.String(),
			},
		},
		State:   StateUnpacked,
		Message: fmt.Sprintf("unpacked %q successfully", ref),

		// We truncate both the unpack time and last successful poll attempt
		// to the second because metav1.Time is serialized
		// as RFC 3339 which only has second-level precision. When we
		// use this result in a comparison with what we deserialized
		// from the Kubernetes API server, we need it to match.
		UnpackTime:                modTime.Truncate(time.Second),
		LastSuccessfulPollAttempt: metav1.NewTime(time.Now().Truncate(time.Second)),
	}, nil
}

func (i *ContainersImageRegistry) Cleanup(ctx context.Context, id string) error {
	return i.Cache.DeleteID(ctx, id)
}
