package imagestream

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	dcontext "github.com/docker/distribution/context"
	"github.com/opencontainers/go-digest"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	imageapiv1 "github.com/openshift/api/image/v1"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/client"
	rerrors "github.com/openshift/image-registry/pkg/errors"
	util "github.com/openshift/image-registry/pkg/origin-common/util"
	"github.com/openshift/library-go/pkg/image/reference"
	"github.com/openshift/library-go/pkg/quota/quotautil"
)

const (
	ErrImageStreamCode              = "ImageStream:"
	ErrImageStreamUnknownErrorCode  = ErrImageStreamCode + "Unknown"
	ErrImageStreamNotFoundCode      = ErrImageStreamCode + "NotFound"
	ErrImageStreamImageNotFoundCode = ErrImageStreamCode + "ImageNotFound"
	ErrImageStreamForbiddenCode     = ErrImageStreamCode + "Forbidden"
)

// ProjectObjectListStore represents a cache of objects indexed by a project name.
// Used to store a list of items per namespace.
type ProjectObjectListStore interface {
	Add(namespace string, obj runtime.Object) error
	Get(namespace string) (obj runtime.Object, exists bool, err error)
}

// ImagePullthroughSpec contains a reference of remote image to pull associated with an insecure flag for the
// corresponding registry.
type ImagePullthroughSpec struct {
	DockerImageReference *reference.DockerImageReference
	Insecure             bool
}

type ImageStream interface {
	Reference() string
	Exists(ctx context.Context) (bool, rerrors.Error)

	GetImageOfImageStream(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, rerrors.Error)
	CreateImageStreamMapping(ctx context.Context, userClient client.Interface, tag string, image *imageapiv1.Image) rerrors.Error
	ResolveImageID(ctx context.Context, dgst digest.Digest) (*imageapiv1.TagEvent, rerrors.Error)

	HasBlob(ctx context.Context, dgst digest.Digest) (bool, *imageapiv1.ImageStreamLayers, *imageapiv1.Image)
	IdentifyCandidateRepositories(ctx context.Context, primary bool) ([]string, map[string]ImagePullthroughSpec, rerrors.Error)
	GetLimitRangeList(ctx context.Context, cache ProjectObjectListStore) (*corev1.LimitRangeList, rerrors.Error)
	GetSecrets() ([]corev1.Secret, rerrors.Error)

	TagIsInsecure(ctx context.Context, tag string, dgst digest.Digest) (bool, rerrors.Error)
	Tags(ctx context.Context) (map[string]digest.Digest, rerrors.Error)
}

type imageStream struct {
	namespace string
	name      string

	registryOSClient client.Interface

	imageClient imageGetter

	// imageStreamGetter fetches and caches an image stream.
	// The image stream stays cached for the entire time of handling single
	// repository-scoped request.
	imageStreamGetter *cachedImageStreamGetter
}

var _ ImageStream = &imageStream{}

func New(ctx context.Context, namespace, name string, client client.Interface) ImageStream {
	return &imageStream{
		namespace:        namespace,
		name:             name,
		registryOSClient: client,
		imageClient:      newCachedImageGetter(client),
		imageStreamGetter: &cachedImageStreamGetter{
			namespace:    namespace,
			name:         name,
			isNamespacer: client,
		},
	}
}

func (is *imageStream) Reference() string {
	return fmt.Sprintf("%s/%s", is.namespace, is.name)
}

// getImage retrieves the Image with digest `dgst`. No authorization check is done.
func (is *imageStream) getImage(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, rerrors.Error) {
	image, err := is.imageClient.Get(ctx, dgst)

	switch {
	case kerrors.IsNotFound(err):
		return nil, rerrors.NewError(
			ErrImageStreamImageNotFoundCode,
			fmt.Sprintf("getImage: unable to find image digest %s in %s", dgst.String(), is.name),
			err,
		)
	case err != nil:
		return nil, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("getImage: unable to get image digest %s in %s", dgst.String(), is.name),
			err,
		)
	}

	return image, nil
}

// ResolveImageID returns latest TagEvent for specified imageID and an error if
// there's more than one image matching the ID or when one does not exist.
func (is *imageStream) ResolveImageID(ctx context.Context, dgst digest.Digest) (*imageapiv1.TagEvent, rerrors.Error) {
	stream, rErr := is.imageStreamGetter.get()

	if rErr != nil {
		return nil, convertImageStreamGetterError(rErr, fmt.Sprintf("ResolveImageID: failed to get image stream %s", is.Reference()))
	}

	tagEvent, err := util.ResolveImageID(stream, dgst.String())
	if err != nil {
		code := ErrImageStreamUnknownErrorCode

		if kerrors.IsNotFound(err) {
			code = ErrImageStreamImageNotFoundCode
		}

		return nil, rerrors.NewError(
			code,
			fmt.Sprintf("ResolveImageID: unable to resolve ImageID %s in image stream %s", dgst.String(), is.Reference()),
			err,
		)
	}

	return tagEvent, nil
}

// getStoredImageOfImageStream retrieves the Image with digest `dgst` and
// ensures that the image belongs to the image stream `is`. It uses two
// queries to master API:
//
//	1st to get a corresponding image stream
//	2nd to get the image
//
// This allows us to cache the image stream for later use.
//
// If you need the image object to be modified according to image stream tag,
// please use GetImageOfImageStream.
func (is *imageStream) getStoredImageOfImageStream(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, *imageapiv1.TagEvent, rerrors.Error) {
	tagEvent, err := is.ResolveImageID(ctx, dgst)
	if err != nil {
		return nil, nil, err
	}

	image, err := is.getImage(ctx, dgst)
	if err != nil {
		return nil, nil, err
	}

	return image, tagEvent, nil
}

func (is *imageStream) getImageOfImageStream(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, rerrors.Error) {
	image, tagEvent, err := is.getStoredImageOfImageStream(ctx, dgst)
	if err != nil {
		return nil, err
	}

	// We don't want to mutate the origial image object, which we've got by reference.
	img := *image
	img.DockerImageReference = tagEvent.DockerImageReference

	return &img, nil
}

// GetImageOfImageStream retrieves the Image with the given digest for the image
// stream. The image's field DockerImageReference is modified on the fly to
// pretend that we've got the image from the source from which the image was
// tagged to match tag's DockerImageReference.
//
// The layers API is also searched, as a manifest which is part of a manifest
// list in an image stream will not be available in the image stream history,
// only its parent manifest list will be found there.
//
// If the Image with the given digest is not part of the image stream, a not found
// error is returned.
//
// NOTE: due to on the fly modification, the returned image object should
// not be sent to the master API. If you need unmodified version of the
// image object, please use getStoredImageOfImageStream.
func (is *imageStream) GetImageOfImageStream(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, rerrors.Error) {
	isImage, err := is.getImageOfImageStream(ctx, dgst)
	if err == nil {
		return isImage, nil
	}

	ref, err := is.resolveUpstreamRef(ctx, dgst)
	if err != nil {
		return nil, err
	}

	image, err := is.getImage(ctx, dgst)
	if err != nil {
		return nil, err
	}

	// We don't want to mutate the origial image object, which we've got by reference.
	img := *image
	img.DockerImageReference = ref.String()

	return &img, nil
}

// resolveUpstreamRef returns an image reference for an image with the given
// digest that can be used to pull the image from the upstream repository.
//
// It uses the image layers API to find the parent image, and then finds the
// upstream repository for the parent image in the image stream.
//
// It works only for sub-manifests, for which the image stream usually does not
// have a history entry. For the main manifest, the image stream should have a
// history entry that can be found by ResolveImageID.
func (is *imageStream) resolveUpstreamRef(ctx context.Context, dgst digest.Digest) (reference.DockerImageReference, rerrors.Error) {
	layers, rErr := is.imageStreamGetter.layers()
	if rErr != nil {
		return reference.DockerImageReference{}, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("resolveUpstreamRef: failed to get layers for image stream %s", is.Reference()),
			rErr,
		)
	}

	parent := ""
	for image, ibr := range layers.Images {
		found := false
		for _, m := range ibr.Manifests {
			if m == dgst.String() {
				found = true
				break
			}
		}
		if found {
			parent = image
			break
		}
	}
	if parent == "" {
		return reference.DockerImageReference{}, rerrors.NewError(
			ErrImageStreamImageNotFoundCode,
			fmt.Sprintf("resolveUpstreamRef: unable to find parent for image %s in image stream %s", dgst.String(), is.Reference()),
			nil,
		)
	}

	parentTagEvent, rErr := is.ResolveImageID(ctx, digest.Digest(parent))
	if rErr != nil {
		return reference.DockerImageReference{}, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("resolveUpstreamRef: unable to get parent event %s in image stream %s", parent, is.Reference()),
			rErr,
		)
	}

	ref, err := reference.Parse(parentTagEvent.DockerImageReference)
	if err != nil {
		return reference.DockerImageReference{}, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("resolveUpstreamRef: unable to parse parent image reference %s in image stream %s", parentTagEvent.DockerImageReference, is.Reference()),
			err,
		)
	}

	ref.Tag = ""
	ref.ID = dgst.String()

	return ref, nil
}

func (is *imageStream) GetSecrets() ([]corev1.Secret, rerrors.Error) {
	secrets, err := is.registryOSClient.ImageStreamSecrets(is.namespace).Secrets(context.TODO(), is.name, metav1.GetOptions{})
	if err != nil {
		return nil, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("GetSecrets: error getting secrets for repository %s", is.Reference()),
			err,
		)
	}
	return secrets.Items, nil
}

// TagIsInsecure returns true if the given image stream or its tag allow for
// insecure transport.
func (is *imageStream) TagIsInsecure(ctx context.Context, tag string, dgst digest.Digest) (bool, rerrors.Error) {
	stream, err := is.imageStreamGetter.get()
	if err != nil {
		return false, convertImageStreamGetterError(err, fmt.Sprintf("TagIsInsecure: failed to get image stream %s", is.Reference()))
	}

	if insecure := stream.Annotations[imageapiv1.InsecureRepositoryAnnotation]; insecure == "true" {
		return true, nil
	}

	if len(tag) == 0 {
		// if the client pulled by digest, find the corresponding tag in the image stream
		tag, _ = util.LatestImageTagEvent(stream, dgst.String())
	}

	if len(tag) != 0 {
		for _, t := range stream.Spec.Tags {
			if t.Name == tag {
				return t.ImportPolicy.Insecure, nil
			}
		}
	}

	return false, nil
}

func (is *imageStream) Exists(ctx context.Context) (bool, rerrors.Error) {
	_, rErr := is.imageStreamGetter.get()
	if rErr != nil {
		if rErr.Code() == ErrImageStreamGetterNotFoundCode {
			return false, nil
		}
		return false, convertImageStreamGetterError(rErr, fmt.Sprintf("Exists: failed to get image stream %s", is.Reference()))
	}
	return true, nil
}

func (is *imageStream) localRegistry(ctx context.Context) ([]string, rerrors.Error) {
	stream, rErr := is.imageStreamGetter.get()
	if rErr != nil {
		return nil, convertImageStreamGetterError(rErr, fmt.Sprintf("localRegistry: failed to get image stream %s", is.Reference()))
	}

	var localNames []string

	local, err := reference.Parse(stream.Status.DockerImageRepository)
	if err != nil {
		dcontext.GetLogger(ctx).Warnf("localRegistry: unable to parse dockerImageRepository %q", stream.Status.DockerImageRepository)
	}
	if len(local.Registry) != 0 {
		localNames = append(localNames, local.Registry)
	}

	if len(stream.Status.PublicDockerImageRepository) > 0 {
		public, err := reference.Parse(stream.Status.PublicDockerImageRepository)
		if err != nil {
			dcontext.GetLogger(ctx).Warnf("localRegistry: unable to parse publicDockerImageRepository %q", stream.Status.PublicDockerImageRepository)
		}
		if len(public.Registry) != 0 {
			localNames = append(localNames, public.Registry)
		}
	}

	return localNames, nil
}

func (is *imageStream) IdentifyCandidateRepositories(ctx context.Context, primary bool) ([]string, map[string]ImagePullthroughSpec, rerrors.Error) {
	stream, err := is.imageStreamGetter.get()
	if err != nil {
		return nil, nil, convertImageStreamGetterError(err, fmt.Sprintf("IdentifyCandidateRepositories: failed to get image stream %s", is.Reference()))
	}

	localRegistry, _ := is.localRegistry(ctx)

	repositoryCandidates, search := identifyCandidateRepositories(stream, localRegistry, primary)
	return repositoryCandidates, search, nil
}

func (is *imageStream) Tags(ctx context.Context) (map[string]digest.Digest, rerrors.Error) {
	stream, err := is.imageStreamGetter.get()
	if err != nil {
		return nil, convertImageStreamGetterError(err, fmt.Sprintf("Tags: failed to get image stream %s", is.Reference()))
	}

	m := make(map[string]digest.Digest)

	for _, history := range stream.Status.Tags {
		if len(history.Items) == 0 {
			continue
		}

		tag := history.Tag

		dgst, err := digest.Parse(history.Items[0].Image)
		if err != nil {
			dcontext.GetLogger(ctx).Errorf("bad digest %s: %v", history.Items[0].Image, err)
			continue
		}

		m[tag] = dgst
	}

	return m, nil
}

func (is *imageStream) CreateImageStreamMapping(ctx context.Context, userClient client.Interface, tag string, image *imageapiv1.Image) rerrors.Error {
	ism := imageapiv1.ImageStreamMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: is.namespace,
			Name:      is.name,
		},
		Image: *image,
		Tag:   tag,
	}

	_, err := is.registryOSClient.ImageStreamMappings(is.namespace).Create(ctx, &ism, metav1.CreateOptions{})

	if err == nil {
		return nil
	}

	if quotautil.IsErrorQuotaExceeded(err) {
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: quota exceeded during creation of %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	// if the error was that the image stream wasn't found, try to auto provision it
	statusErr, ok := err.(*kerrors.StatusError)
	if !ok {
		return rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("CreateImageStreamMapping: error creating %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	status := statusErr.ErrStatus
	isValidKind := false

	if kerrors.IsNotFound(statusErr) && strings.ToLower(status.Details.Kind) == "namespaces" {
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: error creating %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	if status.Details != nil && status.Details.Kind == "imagestreammappings" {
		isValidKind = true
	}
	if !isValidKind || status.Code != http.StatusNotFound || status.Details.Name != is.name {
		return rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("CreateImageStreamMapping: error creation of %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	stream := &imageapiv1.ImageStream{}
	stream.Name = is.name

	_, err = userClient.ImageStreams(is.namespace).Create(ctx, stream, metav1.CreateOptions{})

	switch {
	case kerrors.IsAlreadyExists(err), kerrors.IsConflict(err):
		// It is ok.
	case kerrors.IsForbidden(err), kerrors.IsUnauthorized(err), quotautil.IsErrorQuotaExceeded(err):
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: denied creating ImageStream %s", is.Reference()),
			err,
		)
	case err != nil:
		return rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("CreateImageStreamMapping: error auto provisioning ImageStream %s", is.Reference()),
			err,
		)
	}

	dcontext.GetLogger(ctx).Debugf("cache image stream %s/%s", stream.Namespace, stream.Name)
	is.imageStreamGetter.cacheImageStream(stream)

	// try to create the ISM again
	_, err = is.registryOSClient.ImageStreamMappings(is.namespace).Create(ctx, &ism, metav1.CreateOptions{})

	if err == nil {
		return nil
	}

	if quotautil.IsErrorQuotaExceeded(err) {
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: quota exceeded during creation of %s ImageStreamMapping second time", is.Reference()),
			err,
		)
	}

	return rerrors.NewError(
		ErrImageStreamUnknownErrorCode,
		fmt.Sprintf("CreateImageStreamMapping: error creating %s ImageStreamMapping second time", is.Reference()),
		err,
	)
}

// GetLimitRangeList returns list of limit ranges for repo.
func (is *imageStream) GetLimitRangeList(ctx context.Context, cache ProjectObjectListStore) (*corev1.LimitRangeList, rerrors.Error) {
	if cache != nil {
		obj, exists, _ := cache.Get(is.namespace)
		if exists {
			return obj.(*corev1.LimitRangeList), nil
		}
	}

	dcontext.GetLogger(ctx).Debugf("listing limit ranges in namespace %s", is.namespace)

	lrs, err := is.registryOSClient.LimitRanges(is.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("GetLimitRangeList: failed to list limitranges for %s", is.Reference()),
			err,
		)
	}

	if cache != nil {
		err = cache.Add(is.namespace, lrs)
		if err != nil {
			dcontext.GetLogger(ctx).Errorf("GetLimitRangeList: failed to cache limit range list: %v", err)
		}
	}

	return lrs, nil
}

func convertImageStreamGetterError(err rerrors.Error, msg string) rerrors.Error {
	code := ErrImageStreamUnknownErrorCode

	switch err.Code() {
	case ErrImageStreamGetterNotFoundCode:
		code = ErrImageStreamNotFoundCode
	case ErrImageStreamGetterForbiddenCode:
		code = ErrImageStreamForbiddenCode
	}

	return rerrors.NewError(code, msg, err)
}
