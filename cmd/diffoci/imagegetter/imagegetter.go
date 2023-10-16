package imagegetter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/containerd/containerd/archive/compression"
	ctrimages "github.com/containerd/containerd/cmd/ctr/commands/images"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/pkg/transfer"
	"github.com/containerd/containerd/pkg/transfer/archive"
	"github.com/containerd/containerd/pkg/transfer/image"
	transimage "github.com/containerd/containerd/pkg/transfer/image"
	"github.com/containerd/containerd/pkg/transfer/registry"
	"github.com/containerd/containerd/platforms"
	refdocker "github.com/containerd/containerd/reference/docker"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/reproducible-containers/diffoci/cmd/diffoci/backend"
	"github.com/reproducible-containers/diffoci/pkg/dockercred"
)

func Load(ctx context.Context, stdout io.Writer, transferrer transfer.Transferrer, tarR io.Reader, plats []ocispec.Platform, foreknownRef string) error {
	decompressed, err := compression.DecompressStream(tarR)
	if err != nil {
		return err
	}
	iis := archive.NewImageImportStream(decompressed, "")

	sOpts := []transimage.StoreOpt{
		transimage.WithPlatforms(plats...),
		image.WithPlatforms(plats...),
		image.WithAllMetadata,
		image.WithNamedPrefix("unused", true),
	}
	is := transimage.NewStore(foreknownRef, sOpts...)

	pf, done := ctrimages.ProgressHandler(ctx, stdout)
	defer done()

	if err := transferrer.Transfer(ctx, iis, is, transfer.WithProgress(pf)); err != nil {
		return fmt.Errorf("failed to load: %w", err)
	}
	return nil
}

func Pull(ctx context.Context, stdout io.Writer, transferrer transfer.Transferrer, credHelper registry.CredentialHelper, ref string, plats []ocispec.Platform) error {
	reg := registry.NewOCIRegistry(ref, nil, credHelper)

	sOpts := []transimage.StoreOpt{
		transimage.WithPlatforms(plats...),
	}
	is := transimage.NewStore(ref, sOpts...)

	pf, done := ctrimages.ProgressHandler(ctx, stdout)
	defer done()

	if err := transferrer.Transfer(ctx, reg, is, transfer.WithProgress(pf)); err != nil {
		return fmt.Errorf("failed to pull %q: %w", ref, err)
	}
	return nil
}

type ImageGetter struct {
	progressWriter io.Writer // stderr
	imageStore     images.Store
	contentStore   content.Store
	transferrer    transfer.Transferrer
	credHelper     registry.CredentialHelper
}

func New(progressWriter io.Writer, backend backend.Backend) (*ImageGetter, error) {
	credHelper, err := dockercred.NewCredentialHelper()
	if err != nil {
		return nil, err
	}
	return &ImageGetter{
		progressWriter: progressWriter,
		imageStore:     backend.ImageService(),
		contentStore:   backend.ContentStore(),
		transferrer:    backend,
		credHelper:     credHelper,
	}, nil
}

type PullMode string

const (
	PullAlways  = "always"
	PullMissing = "missing"
	PullNever   = "never"

	dockerImagePrefix = "docker://"
	podmanImagePrefix = "podman://"
)

func (g *ImageGetter) isDocker(rawRef string) bool {
	return strings.HasPrefix(rawRef, dockerImagePrefix)
}

func (g *ImageGetter) isPodman(rawRef string) bool {
	return strings.HasPrefix(rawRef, podmanImagePrefix)
}

func (g *ImageGetter) getDocker(ctx context.Context, rawRef string, plats []ocispec.Platform) (*images.Image, error) {
	rawRefTrimmed := strings.TrimPrefix(rawRef, dockerImagePrefix)
	ref, err := refdocker.ParseDockerRef(rawRefTrimmed)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %q: %w", rawRefTrimmed, err)
	}
	name := ref.String()
	docker := os.Getenv("DOCKER")
	if docker == "" {
		docker = "docker"
	}
	return g.loadDocker(ctx, docker, name, plats)
}

func (g *ImageGetter) getPodman(ctx context.Context, rawRef string, plats []ocispec.Platform) (*images.Image, error) {
	rawRefTrimmed := strings.TrimPrefix(rawRef, podmanImagePrefix)
	ref, err := refdocker.ParseDockerRef(rawRefTrimmed)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %q: %w", rawRefTrimmed, err)
	}
	name := ref.String()
	podman := os.Getenv("PODMAN")
	if podman == "" {
		podman = "podman"
	}
	return g.loadDocker(ctx, podman, name, plats)
}

type readerWithEOF struct {
	io.Reader
}

func (r *readerWithEOF) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	return n, err
}

// loadDocker runs `docker save` and loads the result
func (g *ImageGetter) loadDocker(ctx context.Context, docker, name string, plats []ocispec.Platform) (*images.Image, error) {
	log.G(ctx).Infof("Loading image %q from %q", name, docker)
	dockerCmd := exec.Command(docker, "save", name)
	dockerCmd.Stderr = os.Stderr
	r, err := dockerCmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	log.G(ctx).Debugf("Running %v", dockerCmd.Args)
	if err = dockerCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to run %v: %w", dockerCmd.Args, err)
	}
	if err = Load(ctx, g.progressWriter, g.transferrer, &readerWithEOF{r}, plats, name); err != nil {
		return nil, fmt.Errorf("failed to load an archive (from %v): %w", dockerCmd.Args, err)
	}
	if err = r.Close(); err != nil {
		return nil, err
	}
	img, err := g.imageStore.Get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("should have loaded an archive (from %v), but the loaded image is not accessible: %w", dockerCmd.Args, err)
	}

	// Check platforms
	platMC := platforms.Any(plats...)
	available, _, _, _, err := images.Check(ctx, g.contentStore, img.Target, platMC)
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, fmt.Errorf("image %q lacks blobs for additional platforms: %w", name, errdefs.ErrUnavailable)
	}
	return &img, nil
}

func (g *ImageGetter) Get(ctx context.Context, rawRef string, plats []ocispec.Platform, pullMode PullMode) (*images.Image, error) {
	if g.isDocker(rawRef) {
		return g.getDocker(ctx, rawRef, plats)
	}
	if g.isPodman(rawRef) {
		return g.getPodman(ctx, rawRef, plats)
	}
	ref, err := refdocker.ParseDockerRef(rawRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %q: %w", rawRef, err)
	}
	name := ref.String()

	switch pullMode {
	case PullAlways:
		log.G(ctx).Infof("Pulling %q", name)
		if err := Pull(ctx, g.progressWriter, g.transferrer, g.credHelper, name, plats); err != nil {
			return nil, fmt.Errorf("failed to pull %q: %w", name, err)
		}
	case PullMissing, PullNever:
		// NOP
	default:
		return nil, fmt.Errorf("unknown pull mode %q", pullMode)
	}

	// Get the image object
	img, err := g.imageStore.Get(ctx, name)
	if err != nil {
		if errors.Is(err, errdefs.ErrNotFound) && pullMode != PullNever {
			log.G(ctx).Infof("Pulling %q", name)
			if pullErr := Pull(ctx, g.progressWriter, g.transferrer, g.credHelper, name, plats); pullErr != nil {
				return nil, fmt.Errorf("failed to pull %q: %w", name, pullErr)
			}
			var retryErr error
			img, retryErr = g.imageStore.Get(ctx, name)
			if retryErr != nil {
				return nil, fmt.Errorf("should have pulled %q, but still not accessible in the local store: %w", name, retryErr)
			}
			err = nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get image %q: %w", name, err)
	}

	// Check platforms
	platMC := platforms.Any(plats...)
	available, _, _, _, err := images.Check(ctx, g.contentStore, img.Target, platMC)
	if err != nil {
		return nil, err
	}
	if !available {
		if pullMode == PullNever {
			return nil, fmt.Errorf("image %q lacks blobs for additional platforms: %w", name, errdefs.ErrUnavailable)
		} else {
			log.G(ctx).Infof("Pulling %q for additional platforms", name)
			if err := Pull(ctx, g.progressWriter, g.transferrer, g.credHelper, name, plats); err != nil {
				return nil, fmt.Errorf("failed to pull %q: %w", name, err)
			}
		}
	}
	return &img, nil
}
