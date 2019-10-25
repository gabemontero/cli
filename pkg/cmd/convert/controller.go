package convert

import (
	"bytes"
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	kutilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/apis/policy"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	buildv1 "github.com/openshift/api/build/v1"
	imagev1 "github.com/openshift/api/image/v1"
	"github.com/openshift/library-go/pkg/image/imageutil"
	"github.com/openshift/library-go/pkg/image/reference"
	"github.com/openshift/library-go/pkg/image/referencemutator"
	"github.com/openshift/library-go/pkg/build/naming"
	sharedbuildutil "github.com/openshift/library-go/pkg/build/buildutil"
	"github.com/openshift/library-go/pkg/build/envresolve"
)

const (
	BuilderServiceAccountName = "builder"
	buildPodSuffix           = "build"
	stiBuild = "sti-build"
	BuildWorkDirMount = "/tmp/build"
	BuildBlobsMetaCache = "/var/lib/containers/cache"
	BuildBlobsContentCache = "/var/cache/blobs"
	dockerSocketPath      = "/var/run/docker.sock"
	sourceSecretMountPath = "/var/run/secrets/openshift.io/source"

	DockerPushSecretMountPath            = "/var/run/secrets/openshift.io/push"
	DockerPullSecretMountPath            = "/var/run/secrets/openshift.io/pull"
	ConfigMapBuildSourceBaseMountPath    = "/var/run/configs/openshift.io/build"
	ConfigMapBuildSystemConfigsMountPath = "/var/run/configs/openshift.io/build-system"
	ConfigMapCertsMountPath              = "/var/run/configs/openshift.io/certs"
	SecretBuildSourceBaseMountPath       = "/var/run/secrets/openshift.io/build"
	SourceImagePullSecretMountPath       = "/var/run/secrets/openshift.io/source-image"
	// ConfigMapBuildGlobalCAMountPath is the directory where the tls-ca-bundle.pem file will be mounted
	// by the cluster CA operator
	ConfigMapBuildGlobalCAMountPath = "/etc/pki/ca-trust/extracted/pem"

	// ExtractImageContentContainer is the name of the container that will
	// pull down input images and extract their content for input to the build.
	ExtractImageContentContainer = "extract-image-content"

	// GitCloneContainer is the name of the container that will clone the
	// build source repository and also handle binary input content.
	GitCloneContainer = "git-clone"

	CustomBuild = "custom-build"
	DockerBuild = "docker-build"
	StiBuild    = "sti-build"
	Image = "quay.io/openshift/origin-docker-builder:latest"
	sysConfigConfigMapSuffix = "sys-config"
	caConfigMapSuffix        = "ca"
	globalCAConfigMapSuffix  = "global-ca"
	GlobalCAConfigMapKey = "ca-bundle.crt"
	operator        = '$'
	referenceOpener = '('
	referenceCloser = ')'
)

var (
	// errInvalidImageReferences is a marker error for when a build contains invalid object
	// reference names.
	errInvalidImageReferences = fmt.Errorf("one or more image references were invalid")
	// errNoIntegratedRegistry is a marker error for when the output image points to a registry
	// that cannot be resolved.
	errNoIntegratedRegistry = fmt.Errorf("the integrated registry is not configured")

	defaultDropCaps = []string{
		"KILL",
		"MKNOD",
		"SETGID",
		"SETUID",
	}
	customBuildEncodingScheme       = runtime.NewScheme()
	customBuildEncodingCodecFactory = serializer.NewCodecFactory(customBuildEncodingScheme)
	buildEncodingScheme       = runtime.NewScheme()
	buildEncodingCodecFactory = serializer.NewCodecFactory(buildEncodingScheme)
	buildJSONCodec            runtime.Encoder
	BuildControllerRefKind = buildv1.GroupVersion.WithKind("Build")
	hostPortRegex = regexp.MustCompile("\\.\\.(\\d+)$")
	// Decoder understands groupified and non-groupfied.  It deals in internals for now, but will be updated later
	Decoder runtime.Decoder

	// EncoderScheme can identify types for serialization. We use this for the event recorder and other things that need to
	// identify external kinds.
	EncoderScheme = runtime.NewScheme()
	// Encoder always encodes to groupfied.
	Encoder runtime.Encoder
)

func init() {
	utilruntime.Must(buildv1.Install(buildEncodingScheme))
	buildJSONCodec = buildEncodingCodecFactory.LegacyCodec(buildv1.GroupVersion)
	annotationDecodingScheme := runtime.NewScheme()
	utilruntime.Must(buildv1.Install(annotationDecodingScheme))
	utilruntime.Must(buildv1.DeprecatedInstallWithoutGroup(annotationDecodingScheme))
	annotationDecoderCodecFactory := serializer.NewCodecFactory(annotationDecodingScheme)
	Decoder = annotationDecoderCodecFactory.UniversalDecoder(buildv1.GroupVersion)

	utilruntime.Must(buildv1.Install(EncoderScheme))
	annotationEncoderCodecFactory := serializer.NewCodecFactory(EncoderScheme)
	Encoder = annotationEncoderCodecFactory.LegacyCodec(buildv1.GroupVersion)
	utilruntime.Must(buildv1.Install(customBuildEncodingScheme))
	utilruntime.Must(buildv1.DeprecatedInstallWithoutGroup(customBuildEncodingScheme))
	customBuildEncodingCodecFactory = serializer.NewCodecFactory(customBuildEncodingScheme)
}

func resourceName(namespace, name string) string {
	return namespace + "/" + name
}

func unresolvedImageStreamReferences(m referencemutator.ImageReferenceMutator, defaultNamespace string) ([]string, error) {
	var streams []string
	fn := func(ref *corev1.ObjectReference) error {
		switch ref.Kind {
		case "ImageStreamImage":
			namespace := ref.Namespace
			if len(namespace) == 0 {
				namespace = defaultNamespace
			}
			name, _, ok := imageutil.SplitImageStreamTag(ref.Name)
			if !ok {
				return errInvalidImageReferences
			}
			streams = append(streams, resourceName(namespace, name))
		case "ImageStreamTag":
			namespace := ref.Namespace
			if len(namespace) == 0 {
				namespace = defaultNamespace
			}
			name, _, ok := imageutil.SplitImageStreamTag(ref.Name)
			if !ok {
				return errInvalidImageReferences
			}
			streams = append(streams, resourceName(namespace, name))
		}
		return nil
	}
	errs := m.Mutate(fn)
	if len(errs) > 0 {
		return nil, errInvalidImageReferences
	}
	return streams, nil
}

func (opts *startOptions) resolveImageStreamLocation(ref *corev1.ObjectReference, defaultNamespace string) (string, error) {
	namespace := ref.Namespace
	if len(namespace) == 0 {
		namespace = defaultNamespace
	}

	var (
		name string
		tag  string
	)
	switch ref.Kind {
	case "ImageStreamImage":
		var ok bool
		name, _, ok = imageutil.SplitImageStreamTag(ref.Name)
		if !ok {
			return "", errInvalidImageReferences
		}
		// for backwards compatibility, image stream images will be resolved to the :latest tag
		tag = imageutil.DefaultImageTag
	case "ImageStreamTag":
		var ok bool
		name, tag, ok = imageutil.SplitImageStreamTag(ref.Name)
		if !ok {
			return "", errInvalidImageReferences
		}
	case "ImageStream":
		name = ref.Name
	}

	clients, err := opts.cliparams.Clients()
	if err != nil {
		return "", err
	}
	stream, err := clients.OpenShiftImage.ImageStreams(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return "", err
		}
		return "", fmt.Errorf("the referenced output image stream %s/%s could not be found: %v", namespace, name, err)
	}

	// TODO: this check will not work if the admin installs the registry without restarting the controller, because
	// only a relist from the API server will correct the empty value here (no watch events are sent)
	if len(stream.Status.DockerImageRepository) == 0 {
		return "", errNoIntegratedRegistry
	}

	repo, err := reference.Parse(stream.Status.DockerImageRepository)
	if err != nil {
		return "", fmt.Errorf("the referenced output image stream does not represent a valid reference name: %v", err)
	}
	repo.ID = ""
	repo.Tag = tag
	return repo.Exact(), nil
}


func (opts *startOptions) resolveOutputDockerImageReference(build *buildv1.Build) error {
	ref := build.Spec.Output.To
	if ref == nil || ref.Name == "" {
		return nil
	}

	switch ref.Kind {
	case "ImageStream", "ImageStreamTag":
		newRef, err := opts.resolveImageStreamLocation(ref, build.Namespace)
		if err != nil {
			return err
		}
		*ref = corev1.ObjectReference{Kind: "DockerImage", Name: newRef}
		return nil
	default:
		return nil
	}
}

func resolveImageID(stream *imagev1.ImageStream, imageID string) (*imagev1.TagEvent, error) {
	var event *imagev1.TagEvent
	set := sets.NewString()
	for _, history := range stream.Status.Tags {
		for i := range history.Items {
			tagging := &history.Items[i]
			if imageutil.DigestOrImageMatch(tagging.Image, imageID) {
				event = tagging
				set.Insert(tagging.Image)
			}
		}
	}
	switch len(set) {
	case 1:
		return &imagev1.TagEvent{
			Created:              metav1.Now(),
			DockerImageReference: event.DockerImageReference,
			Image:                event.Image,
		}, nil
	case 0:
		return nil, kerrors.NewNotFound(imagev1.Resource("imagestreamimage"), imageID)
	default:
		return nil, kerrors.NewConflict(imagev1.Resource("imagestreamimage"), imageID, fmt.Errorf("multiple images match the prefix %q: %s", imageID, strings.Join(set.List(), ", ")))
	}
}

func (opts *startOptions) resolveImageStreamImage(ref *corev1.ObjectReference, defaultNamespace string) (*corev1.ObjectReference, error) {
	namespace := ref.Namespace
	if len(namespace) == 0 {
		namespace = defaultNamespace
	}
	name, imageID, ok := imageutil.SplitImageStreamTag(ref.Name)
	if !ok {
		return nil, errInvalidImageReferences
	}
	clients, err := opts.cliparams.Clients()
	if err != nil {
		return nil, err
	}
	stream, err := clients.OpenShiftImage.ImageStreams(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("the referenced image stream %s/%s could not be found: %v", namespace, name, err)
	}
	event, err := resolveImageID(stream, imageID)
	if err != nil {
		return nil, err
	}
	if len(event.DockerImageReference) == 0 {
		return nil, fmt.Errorf("the referenced image stream image %s/%s does not have a pull spec", namespace, ref.Name)
	}
	return &corev1.ObjectReference{Kind: "DockerImage", Name: event.DockerImageReference}, nil
}

func (opts *startOptions) resolveImageStreamTag(ref *corev1.ObjectReference, build *buildv1.Build) (*corev1.ObjectReference, error) {
	namespace := ref.Namespace
	if len(namespace) == 0 {
		namespace = build.Namespace
	}
	name, tag, ok := imageutil.SplitImageStreamTag(ref.Name)
	if !ok {
		return nil, errInvalidImageReferences
	}
	clients, err := opts.cliparams.Clients()
	if err != nil {
		return nil, err
	}
	stream, err := clients.OpenShiftImage.ImageStreams(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("the referenced image stream %s/%s could not be found: %v", namespace, name, err)
	}
	newRef := ""
	ok = false
	if len(stream.Status.DockerImageRepository) > 0 {
		newRef, ok = imageutil.ResolveLatestTaggedImage(stream, tag)
	} /*else {
		streamCopy := stream.DeepCopy()
		// in case the api server has not yet picked up the internal registry hostname from the cluster wide
		// OCM config, we use our copy here to facilitate leveraging pull through with local tag reference policy
		streamCopy.Status.DockerImageRepository = bc.internalRegistryHostname
		newRef, ok = imageutil.ResolveLatestTaggedImage(streamCopy, tag)
	}*/
	if ok {
		// generate informational event if
		// a) tag has spec
		// b) tag is local reference
		// c) status DockerImageRepository is not set, meaning we got this copy of the stream from the watch
		//    before the image registry was set up
		// note, this method is only called for non-output image refs, so we should not get duplicates with
		// the error/warning currently in place for the output link falling into this category
		ref2, ok2 := imageutil.SpecHasTag(stream, tag)
		if ok2 && ref2.ReferencePolicy.Type == imagev1.LocalTagReferencePolicy && len(stream.Status.DockerImageRepository) == 0 {
			klog.Infof("The build %s/%s references an image stream tag %s with the local reference policy type, but the internal image registry location was not marked in the image stream",
				build.Namespace,
				build.Name,
				ref.Name)
		}
		return &corev1.ObjectReference{Kind: "DockerImage", Name: newRef}, nil
	}
	return nil, fmt.Errorf("the referenced image stream tag %s/%s does not exist", namespace, ref.Name)
}

func (opts *startOptions) resolveImageReferences(build *buildv1.Build) error {
	m := referencemutator.NewBuildMutator(build)

	// get a list of all unresolved references to add to the cache
	streams, err := unresolvedImageStreamReferences(m, build.Namespace)
	if err != nil {
		return err
	}
	if len(streams) == 0 {
		klog.V(5).Infof("Build %s contains no unresolved image references", build.Name)
		return nil
	}


	// resolve the output image reference
	if err := opts.resolveOutputDockerImageReference(build); err != nil {
		if err == errNoIntegratedRegistry {
			err = fmt.Errorf("an image stream cannot be used as build output because the integrated container image registry is not configured")
		}
		return err
	}
	// resolve the remaining references
	errs := m.Mutate(func(ref *corev1.ObjectReference) error {
		switch ref.Kind {
		case "ImageStreamImage":
			newRef, err := opts.resolveImageStreamImage(ref, build.Namespace)
			if err != nil {
				return err
			}
			*ref = *newRef
		case "ImageStreamTag":
			newRef, err := opts.resolveImageStreamTag(ref, build)
			if err != nil {
				return err
			}
			*ref = *newRef
		}
		return nil
	})

	if len(errs) > 0 {
		return errs.ToAggregate()
	}
	// we have resolved all images, and will not need any further notifications
	return nil
}

func (opts *startOptions) fetchServiceAccountSecrets(namespace, serviceAccount string) ([]corev1.Secret, error) {
	var result []corev1.Secret
	clients, err := opts.cliparams.Clients()
	if err != nil {
		return nil, err
	}
	sa, err := clients.Kube.CoreV1().ServiceAccounts(namespace).Get(serviceAccount, metav1.GetOptions{})
	if err != nil {
		return result, fmt.Errorf("Error getting push/pull secrets for service account %s/%s: %v", namespace, serviceAccount, err)
	}
	for _, ref := range sa.Secrets {
		secret, err := clients.Kube.CoreV1().Secrets(namespace).Get(ref.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		result = append(result, *secret)
	}
	return result, nil
}

func (opts *startOptions) resolveImageSecretAsReference(build *buildv1.Build, imagename string) (*corev1.LocalObjectReference, error) {
	serviceAccount := build.Spec.ServiceAccount
	if len(serviceAccount) == 0 {
		serviceAccount = BuilderServiceAccountName
	}
	builderSecrets, err := opts.fetchServiceAccountSecrets(build.Namespace, serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("Error getting push/pull secrets for service account %s/%s: %v", build.Namespace, serviceAccount, err)
	}
	var secret *corev1.LocalObjectReference
	if len(imagename) != 0 {
		secret = findDockerSecretAsReference(builderSecrets, imagename)
	}
	if secret == nil {
		klog.V(4).Infof("build %s is referencing an unknown image, will attempt to use the default secret for the service account", build.Name)
		dockerSecretExists := false
		for _, builderSecret := range builderSecrets {
			if builderSecret.Type == corev1.SecretTypeDockercfg || builderSecret.Type == corev1.SecretTypeDockerConfigJson {
				dockerSecretExists = true
				secret = &corev1.LocalObjectReference{Name: builderSecret.Name}
				break
			}
		}
		// If there are no docker secrets associated w/ the service account, return an error so the build
		// will be retried.  The secrets will be created shortly.
		if !dockerSecretExists {
			return nil, fmt.Errorf("No docker secrets associated with build service account %s", serviceAccount)
		}
		klog.V(4).Infof("No secrets found for pushing or pulling image named %s for build, using default: %s %s/%s", imagename, build.Namespace, build.Name, secret.Name)
	}
	return secret, nil
}

func addSourceEnvVars(source buildv1.BuildSource, output *[]corev1.EnvVar) {
	sourceVars := []corev1.EnvVar{}
	if source.Git != nil {
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_REPOSITORY", Value: source.Git.URI})
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_URI", Value: source.Git.URI})
	}
	if len(source.ContextDir) > 0 {
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_CONTEXT_DIR", Value: source.ContextDir})
	}
	if source.Git != nil && len(source.Git.Ref) > 0 {
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_REF", Value: source.Git.Ref})
	}
	*output = append(*output, sourceVars...)
}

func mergeEnvWithoutDuplicates(source []corev1.EnvVar, output *[]corev1.EnvVar, sourcePrecedence bool) {
	whitelist := buildv1.WhitelistEnvVarNames
	// filter out all environment variables except trusted/well known
	// values, because we do not want random environment variables being
	// fed into the privileged STI container via the BuildConfig definition.

	filteredSourceMap := make(map[string]corev1.EnvVar)
	for _, env := range source {
		allowed := false
		if len(whitelist) == 0 {
			allowed = true
		} else {
			for _, acceptable := range buildv1.WhitelistEnvVarNames {
				if env.Name == acceptable {
					allowed = true
					break
				}
			}
		}
		if allowed {
			filteredSourceMap[env.Name] = env
		}
	}
	result := *output
	for i, env := range result {
		// If the value exists in output, optionally override it and remove it
		// from the source list
		if v, found := filteredSourceMap[env.Name]; found {
			if sourcePrecedence {
				result[i].Value = v.Value
			}
			delete(filteredSourceMap, env.Name)
		}
	}

	// iterate the original list so we retain the order of the inputs
	// when we append them to the output.
	for _, v := range source {
		if v, ok := filteredSourceMap[v.Name]; ok {
			result = append(result, v)
		}
	}
	*output = result
}

func (opts *startOptions) canRunAsRoot(build *buildv1.Build) bool {
	//TODO today this leverages the openshift security APIs for
	// pod security policy subject reviews that leverages the build-controller's
	// SA .... do we just no allow this, or expose as a flag on the convert command
	return false
}

func copyEnvVarSlice(in []corev1.EnvVar) []corev1.EnvVar {
	out := make([]corev1.EnvVar, len(in))
	copy(out, in)
	return out
}

func mountSecretVolume(pod *corev1.Pod, container *corev1.Container, secretName, mountPath, volumeSuffix string) {
	mountVolume(pod, container, secretName, mountPath, volumeSuffix, policy.Secret)
}

// mountVolume is a helper method responsible for mounting volumes into a pod.
// The following file system types for the volume are supported:
//
// 1. ConfigMap
// 2. EmptyDir
// 3. Secret
func mountVolume(pod *corev1.Pod, container *corev1.Container, objName, mountPath, volumeSuffix string, fsType policy.FSType) {
	volumeName := naming.GetName(objName, volumeSuffix, kvalidation.DNS1123LabelMaxLength)

	// coerce from RFC1123 subdomain to RFC1123 label.
	volumeName = strings.Replace(volumeName, ".", "-", -1)

	volumeExists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == volumeName {
			volumeExists = true
			break
		}
	}
	mode := int32(0600)
	if !volumeExists {
		volume := makeVolume(volumeName, objName, mode, fsType)
		pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
	}

	volumeMount := corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPath,
		ReadOnly:  true,
	}
	container.VolumeMounts = append(container.VolumeMounts, volumeMount)
}
func makeVolume(volumeName, refName string, mode int32, fsType policy.FSType) corev1.Volume {
	// TODO: Add support for key-based paths for secrets and configMaps?
	vol := corev1.Volume{
		Name:         volumeName,
		VolumeSource: corev1.VolumeSource{},
	}
	switch fsType {
	case policy.ConfigMap:
		vol.VolumeSource.ConfigMap = &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: refName,
			},
			DefaultMode: &mode,
		}
	case policy.EmptyDir:
		vol.VolumeSource.EmptyDir = &corev1.EmptyDirVolumeSource{}
	case policy.Secret:
		vol.VolumeSource.Secret = &corev1.SecretVolumeSource{
			SecretName:  refName,
			DefaultMode: &mode,
		}
	default:
		klog.V(3).Infof("File system %s is not supported for volumes. Using empty directory instead.", fsType)
		vol.VolumeSource.EmptyDir = &corev1.EmptyDirVolumeSource{}
	}

	return vol
}

func setupSourceSecrets(pod *corev1.Pod, container *corev1.Container, sourceSecret *corev1.LocalObjectReference) {
	if sourceSecret == nil {
		return
	}

	mountSecretVolume(pod, container, sourceSecret.Name, sourceSecretMountPath, "source")
	klog.V(3).Infof("Installed source secrets in %s, in Pod %s/%s", sourceSecretMountPath, pod.Namespace, pod.Name)
	container.Env = append(container.Env, []corev1.EnvVar{
		{Name: "SOURCE_SECRET_PATH", Value: sourceSecretMountPath},
	}...)
}

func setupDockerSecrets(pod *corev1.Pod, container *corev1.Container, pushSecret, pullSecret *corev1.LocalObjectReference, imageSources []buildv1.ImageSource) {
	if pushSecret != nil {
		mountSecretVolume(pod, container, pushSecret.Name, DockerPushSecretMountPath, "push")
		container.Env = append(container.Env, []corev1.EnvVar{
			{Name: "PUSH_DOCKERCFG_PATH", Value: DockerPushSecretMountPath},
		}...)
		klog.V(3).Infof("%s will be used for docker push in %s", DockerPushSecretMountPath, pod.Name)
	}

	if pullSecret != nil {
		mountSecretVolume(pod, container, pullSecret.Name, DockerPullSecretMountPath, "pull")
		container.Env = append(container.Env, []corev1.EnvVar{
			{Name: "PULL_DOCKERCFG_PATH", Value: DockerPullSecretMountPath},
		}...)
		klog.V(3).Infof("%s will be used for docker pull in %s", DockerPullSecretMountPath, pod.Name)
	}

	for i, imageSource := range imageSources {
		if imageSource.PullSecret == nil {
			continue
		}
		mountPath := filepath.Join(SourceImagePullSecretMountPath, strconv.Itoa(i))
		mountSecretVolume(pod, container, imageSource.PullSecret.Name, mountPath, fmt.Sprintf("%s%d", "source-image", i))
		container.Env = append(container.Env, []corev1.EnvVar{
			{Name: fmt.Sprintf("%s%d", "PULL_SOURCE_DOCKERCFG_PATH_", i), Value: mountPath},
		}...)
		klog.V(3).Infof("%s will be used for docker pull in %s", mountPath, pod.Name)
	}
}

func setupContainersStorage(pod *corev1.Pod, container *corev1.Container) {
	exists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "container-storage-root" {
			exists = true
			break
		}
	}
	if !exists {
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: "container-storage-root",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		)
	}
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{
			Name:      "container-storage-root",
			MountPath: "/var/lib/containers/storage",
		},
	)
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_STORAGE_DRIVER", Value: "overlay"})
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_ISOLATION", Value: "chroot"})
}

func makeOwnerReference(build *buildv1.Build) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion: BuildControllerRefKind.GroupVersion().String(),
		Kind:       BuildControllerRefKind.Kind,
		Name:       build.Name,
		UID:        build.UID,
		Controller: &t,
	}
}

func setOwnerReference(pod *corev1.Pod, build *buildv1.Build) {
	pod.OwnerReferences = []metav1.OwnerReference{makeOwnerReference(build)}
}

func mountConfigMapVolume(pod *corev1.Pod, container *corev1.Container, configMapName, mountPath, volumeSuffix string) {
	mountVolume(pod, container, configMapName, mountPath, volumeSuffix, policy.ConfigMap)
}

func setupInputConfigMaps(pod *corev1.Pod, container *corev1.Container, configs []buildv1.ConfigMapBuildSource) {
	for _, c := range configs {
		mountConfigMapVolume(pod, container, c.ConfigMap.Name, filepath.Join(ConfigMapBuildSourceBaseMountPath, c.ConfigMap.Name), "build")
		klog.V(3).Infof("%s will be used as a build config in %s", c.ConfigMap.Name, ConfigMapBuildSourceBaseMountPath)
	}
}

// setupInputSecrets mounts the secrets referenced by the SecretBuildSource
// into a builder container.
func setupInputSecrets(pod *corev1.Pod, container *corev1.Container, secrets []buildv1.SecretBuildSource) {
	for _, s := range secrets {
		mountSecretVolume(pod, container, s.Secret.Name, filepath.Join(SecretBuildSourceBaseMountPath, s.Secret.Name), "build")
		klog.V(3).Infof("%s will be used as a build secret in %s", s.Secret.Name, SecretBuildSourceBaseMountPath)
	}
}

func GetBuildSystemConfigMapName(build *buildv1.Build) string {
	return naming.GetConfigMapName(build.Name, sysConfigConfigMapSuffix)
}

func updateConfigsForContainer(c corev1.Container, volumeName string, configDir string) corev1.Container {
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{
			Name:      volumeName,
			MountPath: configDir,
			ReadOnly:  true,
		},
	)
	// registries.conf is the primary registry config file mounted in by OpenShift
	registriesConfPath := filepath.Join(configDir, buildv1.RegistryConfKey)

	// policy.json sets image policies for buildah (allowed repositories for image pull/push, etc.)
	signaturePolicyPath := filepath.Join(configDir, buildv1.SignaturePolicyKey)

	// registries.d is a directory used by buildah to support multiple registries.conf files
	// currently not created/managed by OpenShift
	registriesDirPath := filepath.Join(configDir, "registries.d")

	// storage.conf configures storage policies for buildah
	// currently not created/managed by OpenShift
	storageConfPath := filepath.Join(configDir, "storage.conf")

	// Setup environment variables for buildah
	// If these paths do not exist in the build container, buildah falls back to sane defaults.
	c.Env = append(c.Env, corev1.EnvVar{Name: "BUILD_REGISTRIES_CONF_PATH", Value: registriesConfPath})
	c.Env = append(c.Env, corev1.EnvVar{Name: "BUILD_REGISTRIES_DIR_PATH", Value: registriesDirPath})
	c.Env = append(c.Env, corev1.EnvVar{Name: "BUILD_SIGNATURE_POLICY_PATH", Value: signaturePolicyPath})
	c.Env = append(c.Env, corev1.EnvVar{Name: "BUILD_STORAGE_CONF_PATH", Value: storageConfPath})
	return c
}

func setupContainersConfigs(build *buildv1.Build, pod *corev1.Pod) {
	const volumeName = "build-system-configs"
	const configDir = ConfigMapBuildSystemConfigsMountPath
	exists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == volumeName {
			exists = true
			break
		}
	}
	if !exists {
		cmSource := &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: GetBuildSystemConfigMapName(build),
			},
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: cmSource,
				},
			},
		)
		containers := make([]corev1.Container, len(pod.Spec.Containers))
		for i, c := range pod.Spec.Containers {
			containers[i] = updateConfigsForContainer(c, volumeName, configDir)
		}
		pod.Spec.Containers = containers
		if len(pod.Spec.InitContainers) > 0 {
			initContainers := make([]corev1.Container, len(pod.Spec.InitContainers))
			for i, c := range pod.Spec.InitContainers {
				initContainers[i] = updateConfigsForContainer(c, volumeName, configDir)
			}
			pod.Spec.InitContainers = initContainers
		}
	}
}

func setupBuildCAs(build *buildv1.Build, pod *corev1.Pod, additionalCAs map[string]string, internalRegistryHost string) {
	casExist := false
	globalCAsExist := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "build-ca-bundles" {
			casExist = true
		}
		if v.Name == "build-proxy-ca-bundles" {
			globalCAsExist = true
		}

		if casExist && globalCAsExist {
			break
		}
	}

	if !casExist {
		// Mount the service signing CA key for the internal registry.
		// This will be injected into the referenced ConfigMap via the openshift/service-ca-operator, and block
		// creation of the build pod until it exists.
		//
		// See https://github.com/openshift/service-serving-cert-signer
		cmSource := &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: naming.GetConfigMapName(build.Name, caConfigMapSuffix),
			},
			Items: []corev1.KeyToPath{
				{
					Key:  buildv1.ServiceCAKey,
					Path: fmt.Sprintf("certs.d/%s/ca.crt", internalRegistryHost),
				},
			},
		}

		// Mount any additional trusted certificates via their keys.
		// Each key should be the hostname that the CA certificate applies to
		// This will be mounted to certs.d/<domain>/ca.crt so that it can be copied
		// to /etc/docker/certs.d
		for key := range additionalCAs {
			// Replace "..[port]" with ":[port]" due to limiations with ConfigMap key names
			mountDir := hostPortRegex.ReplaceAllString(key, ":$1")
			cmSource.Items = append(cmSource.Items, corev1.KeyToPath{
				Key:  key,
				Path: fmt.Sprintf("certs.d/%s/ca.crt", mountDir),
			})
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: "build-ca-bundles",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: cmSource,
				},
			},
		)
		containers := make([]corev1.Container, len(pod.Spec.Containers))
		for i, c := range pod.Spec.Containers {
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{
					Name:      "build-ca-bundles",
					MountPath: ConfigMapCertsMountPath,
				},
			)
			containers[i] = c
		}
		pod.Spec.Containers = containers
	}

	if !globalCAsExist {
		cmSource := &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: naming.GetConfigMapName(build.Name, globalCAConfigMapSuffix),
			},
			//the TBD global CA injector will update the ConfigMapVolumeSource keyToPath items
			Items: []corev1.KeyToPath{
				{
					Key:  GlobalCAConfigMapKey,
					Path: "tls-ca-bundle.pem",
				},
			},
		}

		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: "build-proxy-ca-bundles",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: cmSource,
				},
			},
		)
		containers := make([]corev1.Container, len(pod.Spec.Containers))
		for i, c := range pod.Spec.Containers {
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{
					Name:      "build-proxy-ca-bundles",
					MountPath: ConfigMapBuildGlobalCAMountPath,
				},
			)
			containers[i] = c
		}
		pod.Spec.Containers = containers
		initContainers := make([]corev1.Container, len(pod.Spec.InitContainers))
		for i, c := range pod.Spec.InitContainers {
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{
					Name:      "build-proxy-ca-bundles",
					MountPath: ConfigMapBuildGlobalCAMountPath,
				},
			)
			initContainers[i] = c
		}
	}
}

func setupBlobCache(pod *corev1.Pod) {
	const volume = "build-blob-cache"
	const mountPath = BuildBlobsContentCache
	exists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == volume {
			exists = true
			break
		}
	}
	if !exists {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: volume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
		containers := make([]corev1.Container, len(pod.Spec.Containers))
		for i, c := range pod.Spec.Containers {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name:      volume,
				MountPath: mountPath,
			})
			c.Env = append(c.Env, corev1.EnvVar{
				Name:  "BUILD_BLOBCACHE_DIR",
				Value: mountPath,
			})
			containers[i] = c
		}
		pod.Spec.Containers = containers

		initContainers := make([]corev1.Container, len(pod.Spec.InitContainers))
		for i, ic := range pod.Spec.InitContainers {
			ic.VolumeMounts = append(ic.VolumeMounts, corev1.VolumeMount{
				Name:      volume,
				MountPath: mountPath,
			})
			ic.Env = append(ic.Env, corev1.EnvVar{
				Name:  "BUILD_BLOBCACHE_DIR",
				Value: mountPath,
			})
			initContainers[i] = ic
		}
		pod.Spec.InitContainers = initContainers
	}
}

type FatalError struct {
	// Reason the fatal error occurred
	Reason string
}

func (e *FatalError) Error() string {
	return fmt.Sprintf("fatal error: %s", e.Reason)
}

// IsFatal returns true if the error is fatal
func IsFatal(err error) bool {
	_, isFatal := err.(*FatalError)
	return isFatal
}

func addOutputEnvVars(buildOutput *corev1.ObjectReference, output *[]corev1.EnvVar) error {
	if buildOutput == nil {
		return nil
	}

	// output must always be a DockerImage type reference at this point.
	if buildOutput.Kind != "DockerImage" {
		return fmt.Errorf("invalid build output kind %s, must be DockerImage", buildOutput.Kind)
	}
	ref, err := reference.Parse(buildOutput.Name)
	if err != nil {
		return err
	}
	registry := ref.Registry
	ref.Registry = ""
	image := ref.String()

	outputVars := []corev1.EnvVar{
		{Name: "OUTPUT_REGISTRY", Value: registry},
		{Name: "OUTPUT_IMAGE", Value: image},
	}

	*output = append(*output, outputVars...)
	return nil
}

func setupDockerSocket(pod *corev1.Pod) {
	dockerSocketVolume := corev1.Volume{
		Name: "docker-socket",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: dockerSocketPath,
			},
		},
	}

	dockerSocketVolumeMount := corev1.VolumeMount{
		Name:      "docker-socket",
		MountPath: dockerSocketPath,
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes,
		dockerSocketVolume)
	pod.Spec.Containers[0].VolumeMounts =
		append(pod.Spec.Containers[0].VolumeMounts,
			dockerSocketVolumeMount)
	for i, initContainer := range pod.Spec.InitContainers {
		if initContainer.Name == ExtractImageContentContainer {
			pod.Spec.InitContainers[i].VolumeMounts = append(pod.Spec.InitContainers[i].VolumeMounts, dockerSocketVolumeMount)
			break
		}
	}
}

func setupAdditionalSecrets(pod *corev1.Pod, container *corev1.Container, secrets []buildv1.SecretSpec) {
	for _, secretSpec := range secrets {
		mountSecretVolume(pod, container, secretSpec.SecretSource.Name, secretSpec.MountPath, "secret")
		klog.V(3).Infof("Installed additional secret in %s, in Pod %s/%s", secretSpec.MountPath, pod.Namespace, pod.Name)
	}
}

func (opts *startOptions) createCustomBuildPod(build *buildv1.Build, additionalCAs map[string]string, internalRegistryHost string) (*corev1.Pod, error) {
	strategy := build.Spec.Strategy.CustomStrategy
	if strategy == nil {
		return nil, errors.New("CustomBuildStrategy cannot be executed without CustomStrategy parameters")
	}

	codec := customBuildEncodingCodecFactory.LegacyCodec(buildv1.GroupVersion)
	if len(strategy.BuildAPIVersion) != 0 {
		gv, err := schema.ParseGroupVersion(strategy.BuildAPIVersion)
		if err != nil {
			return nil, &FatalError{fmt.Sprintf("failed to parse buildAPIVersion specified in custom build strategy (%q): %v", strategy.BuildAPIVersion, err)}
		}
		codec = customBuildEncodingCodecFactory.LegacyCodec(gv)
	}

	data, err := runtime.Encode(codec, build)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the build: %v", err)
	}

	containerEnv := []corev1.EnvVar{
		{Name: "BUILD", Value: string(data)},
		{Name: "LANG", Value: "en_US.utf8"},
	}

	if build.Spec.Source.Git != nil {
		addSourceEnvVars(build.Spec.Source, &containerEnv)
	}

	if build.Spec.Output.To != nil {
		addOutputEnvVars(build.Spec.Output.To, &containerEnv)
		if err != nil {
			return nil, fmt.Errorf("failed to parse the output docker tag %q: %v", build.Spec.Output.To.Name, err)
		}
	}

	if len(strategy.From.Name) == 0 {
		return nil, errors.New("CustomBuildStrategy cannot be executed without image")
	}

	if len(strategy.Env) > 0 {
		containerEnv = append(containerEnv, strategy.Env...)
	}

	if strategy.ExposeDockerSocket {
		klog.V(2).Infof("ExposeDockerSocket is enabled for %s build", build.Name)
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "DOCKER_SOCKET", Value: dockerSocketPath})
	}

	serviceAccount := build.Spec.ServiceAccount
	if len(serviceAccount) == 0 {
		serviceAccount = BuilderServiceAccountName
	}

	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.GetPodName(build.Name, buildPodSuffix),
			Namespace: build.Namespace,
			Labels:    map[string]string{buildv1.BuildLabel: labelValue(build.Name)},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{
				{
					Name:  CustomBuild,
					Image: strategy.From.Name,
					Env:   containerEnv,
					// TODO: run unprivileged https://github.com/openshift/origin/issues/662
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  build.Spec.NodeSelector,
		},
	}
	if build.Spec.CompletionDeadlineSeconds != nil {
		pod.Spec.ActiveDeadlineSeconds = build.Spec.CompletionDeadlineSeconds
	}

	if !strategy.ForcePull {
		pod.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	} else {
		klog.V(2).Infof("ForcePull is enabled for %s build", build.Name)
		pod.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
	}
	pod.Spec.Containers[0].Resources = build.Spec.Resources
	if build.Spec.Source.Binary != nil {
		pod.Spec.Containers[0].Stdin = true
		pod.Spec.Containers[0].StdinOnce = true
	}

	if strategy.ExposeDockerSocket {
		setupDockerSocket(pod)
	}
	setupDockerSecrets(pod, &pod.Spec.Containers[0], build.Spec.Output.PushSecret, strategy.PullSecret, build.Spec.Source.Images)
	setOwnerReference(pod, build)
	setupSourceSecrets(pod, &pod.Spec.Containers[0], build.Spec.Source.SourceSecret)
	setupInputSecrets(pod, &pod.Spec.Containers[0], build.Spec.Source.Secrets)
	setupAdditionalSecrets(pod, &pod.Spec.Containers[0], build.Spec.Strategy.CustomStrategy.Secrets)
	setupContainersConfigs(build, pod)
	setupBuildCAs(build, pod, additionalCAs, internalRegistryHost)
	setupContainersStorage(pod, &pod.Spec.Containers[0]) // for unprivileged builds
	return pod, nil
}

func (opts *startOptions) createDockerBuildPod(build *buildv1.Build, additionalCAs map[string]string, internalRegistryHost string) (*corev1.Pod, error) {
	data, err := runtime.Encode(buildJSONCodec, build)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the build: %v", err)
	}

	privileged := true
	strategy := build.Spec.Strategy.DockerStrategy

	containerEnv := []corev1.EnvVar{
		{Name: "BUILD", Value: string(data)},
		{Name: "LANG", Value: "en_US.utf8"},
	}

	addSourceEnvVars(build.Spec.Source, &containerEnv)

	if len(strategy.Env) > 0 {
		mergeEnvWithoutDuplicates(strategy.Env, &containerEnv, true)
	}

	serviceAccount := build.Spec.ServiceAccount
	if len(serviceAccount) == 0 {
		serviceAccount = BuilderServiceAccountName
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.GetPodName(build.Name, buildPodSuffix),
			Namespace: build.Namespace,
			Labels:    map[string]string{buildv1.BuildLabel: labelValue(build.Name)},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{
				{
					Name:    DockerBuild,
					Image:   Image,
					Command: []string{"openshift-docker-build"},
					Env:     copyEnvVarSlice(containerEnv),
					// TODO: run unprivileged https://github.com/openshift/origin/issues/662
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "buildworkdir",
							MountPath: BuildWorkDirMount,
						},
						{
							Name:      "buildcachedir",
							MountPath: BuildBlobsMetaCache,
						},
					},
					ImagePullPolicy: corev1.PullIfNotPresent,
					Resources:       build.Spec.Resources,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "buildcachedir",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: BuildBlobsMetaCache},
					},
				},
				{
					Name: "buildworkdir",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  build.Spec.NodeSelector,
		},
	}

	// can't conditionalize the manage-dockerfile init container because we don't
	// know until we've cloned, whether or not we've got a dockerfile to manage
	// (also if it's a docker type build, we should always have a dockerfile to manage)
	if build.Spec.Source.Git != nil || build.Spec.Source.Binary != nil {
		gitCloneContainer := corev1.Container{
			Name:                     GitCloneContainer,
			Image:                    Image,
			Command:                  []string{"openshift-git-clone"},
			Env:                      copyEnvVarSlice(containerEnv),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "buildworkdir",
					MountPath: BuildWorkDirMount,
				},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       build.Spec.Resources,
		}
		if build.Spec.Source.Binary != nil {
			gitCloneContainer.Stdin = true
			gitCloneContainer.StdinOnce = true
		}
		setupSourceSecrets(pod, &gitCloneContainer, build.Spec.Source.SourceSecret)
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, gitCloneContainer)
	}
	if len(build.Spec.Source.Images) > 0 {
		extractImageContentContainer := corev1.Container{
			Name:    ExtractImageContentContainer,
			Image:   Image,
			Command: []string{"openshift-extract-image-content"},
			Env:     copyEnvVarSlice(containerEnv),
			// TODO: run unprivileged https://github.com/openshift/origin/issues/662
			SecurityContext: &corev1.SecurityContext{
				Privileged: &privileged,
			},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "buildworkdir",
					MountPath: BuildWorkDirMount,
				},
				{
					Name:      "buildcachedir",
					MountPath: BuildBlobsMetaCache,
				},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       build.Spec.Resources,
		}
		setupDockerSecrets(pod, &extractImageContentContainer, build.Spec.Output.PushSecret, strategy.PullSecret, build.Spec.Source.Images)
		setupContainersStorage(pod, &extractImageContentContainer)
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, extractImageContentContainer)
	}
	pod.Spec.InitContainers = append(pod.Spec.InitContainers,
		corev1.Container{
			Name:                     "manage-dockerfile",
			Image:                    Image,
			Command:                  []string{"openshift-manage-dockerfile"},
			Env:                      copyEnvVarSlice(containerEnv),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "buildworkdir",
					MountPath: BuildWorkDirMount,
				},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       build.Spec.Resources,
		},
	)

	if build.Spec.CompletionDeadlineSeconds != nil {
		pod.Spec.ActiveDeadlineSeconds = build.Spec.CompletionDeadlineSeconds
	}

	setOwnerReference(pod, build)
	setupDockerSecrets(pod, &pod.Spec.Containers[0], build.Spec.Output.PushSecret, strategy.PullSecret, build.Spec.Source.Images)
	// For any secrets the user wants to reference from their Assemble script or Dockerfile, mount those
	// secrets into the main container.  The main container includes logic to copy them from the mounted
	// location into the working directory.
	// TODO: consider moving this into the git-clone container and doing the secret copying there instead.
	setupInputSecrets(pod, &pod.Spec.Containers[0], build.Spec.Source.Secrets)
	setupInputConfigMaps(pod, &pod.Spec.Containers[0], build.Spec.Source.ConfigMaps)
	setupContainersConfigs(build, pod)
	setupBuildCAs(build, pod, additionalCAs, internalRegistryHost)
	setupContainersStorage(pod, &pod.Spec.Containers[0]) // for unprivileged builds
	// setupContainersNodeStorage(pod, &pod.Spec.Containers[0]) // for privileged builds
	setupBlobCache(pod)
	return pod, nil
}

func (opts *startOptions) createSourceBuildPod(build *buildv1.Build, additionalCAs map[string]string, internalRegistryHost string) (*corev1.Pod, error) {
	data, err := runtime.Encode(buildJSONCodec, build)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the Build %s/%s: %v", build.Namespace, build.Name, err)
	}

	containerEnv := []corev1.EnvVar{
		{Name: "BUILD", Value: string(data)},
		{Name: "LANG", Value: "en_US.utf8"},
	}

	addSourceEnvVars(build.Spec.Source, &containerEnv)

	strategy := build.Spec.Strategy.SourceStrategy
	if len(strategy.Env) > 0 {
		mergeEnvWithoutDuplicates(strategy.Env, &containerEnv, true)
	}

	// check if can run container as root
	if !opts.canRunAsRoot(build) {
		// TODO: both AllowedUIDs and DropCapabilities should
		// be controlled via the SCC that's in effect for the build service account
		// For now, both are hard-coded based on whether the build service account can
		// run as root.
		containerEnv = append(containerEnv, corev1.EnvVar{Name: buildv1.AllowedUIDs, Value: "1-"})
		containerEnv = append(containerEnv, corev1.EnvVar{Name: buildv1.DropCapabilities, Value: strings.Join(defaultDropCaps, ",")})
	}

	serviceAccount := build.Spec.ServiceAccount
	if len(serviceAccount) == 0 {
		serviceAccount = BuilderServiceAccountName
	}

	privileged := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.GetPodName(build.Name, buildPodSuffix),
			Namespace: build.Namespace,
			Labels:    map[string]string{buildv1.BuildLabel: labelValue(build.Name)},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{
				{
					Name:    stiBuild,
					Image:   Image,
					Command: []string{"openshift-sti-build"},
					Env:     copyEnvVarSlice(containerEnv),
					// TODO: run unprivileged https://github.com/openshift/origin/issues/662
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "buildworkdir",
							MountPath: BuildWorkDirMount,
						},
						{
							Name:      "buildcachedir",
							MountPath: BuildBlobsMetaCache,
						},
					},
					ImagePullPolicy: corev1.PullIfNotPresent,
					Resources:       build.Spec.Resources,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "buildcachedir",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: BuildBlobsMetaCache},
					},
				},
				{
					Name: "buildworkdir",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  build.Spec.NodeSelector,
		},
	}

	if build.Spec.Source.Git != nil || build.Spec.Source.Binary != nil {
		gitCloneContainer := corev1.Container{
			Name:                     GitCloneContainer,
			Image:                    Image,
			Command:                  []string{"openshift-git-clone"},
			Env:                      copyEnvVarSlice(containerEnv),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "buildworkdir",
					MountPath: BuildWorkDirMount,
				},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       build.Spec.Resources,
		}
		if build.Spec.Source.Binary != nil {
			gitCloneContainer.Stdin = true
			gitCloneContainer.StdinOnce = true
		}
		setupSourceSecrets(pod, &gitCloneContainer, build.Spec.Source.SourceSecret)
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, gitCloneContainer)
	}
	if len(build.Spec.Source.Images) > 0 {
		extractImageContentContainer := corev1.Container{
			Name:    ExtractImageContentContainer,
			Image:   Image,
			Command: []string{"openshift-extract-image-content"},
			Env:     copyEnvVarSlice(containerEnv),
			// TODO: run unprivileged https://github.com/openshift/origin/issues/662
			SecurityContext: &corev1.SecurityContext{
				Privileged: &privileged,
			},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "buildworkdir",
					MountPath: BuildWorkDirMount,
				},
				{
					Name:      "buildcachedir",
					MountPath: BuildBlobsMetaCache,
				},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       build.Spec.Resources,
		}
		setupDockerSecrets(pod, &extractImageContentContainer, build.Spec.Output.PushSecret, strategy.PullSecret, build.Spec.Source.Images)
		setupContainersStorage(pod, &extractImageContentContainer)
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, extractImageContentContainer)
	}
	pod.Spec.InitContainers = append(pod.Spec.InitContainers,
		corev1.Container{
			Name:                     "manage-dockerfile",
			Image:                    Image,
			Command:                  []string{"openshift-manage-dockerfile"},
			Env:                      copyEnvVarSlice(containerEnv),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "buildworkdir",
					MountPath: BuildWorkDirMount,
				},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       build.Spec.Resources,
		},
	)

	if build.Spec.CompletionDeadlineSeconds != nil {
		pod.Spec.ActiveDeadlineSeconds = build.Spec.CompletionDeadlineSeconds
	}

	setOwnerReference(pod, build)
	setupDockerSecrets(pod, &pod.Spec.Containers[0], build.Spec.Output.PushSecret, strategy.PullSecret, build.Spec.Source.Images)
	// For any secrets the user wants to reference from their Assemble script or Dockerfile, mount those
	// secrets into the main container.  The main container includes logic to copy them from the mounted
	// location into the working directory.
	// TODO: consider moving this into the git-clone container and doing the secret copying there instead.
	setupInputSecrets(pod, &pod.Spec.Containers[0], build.Spec.Source.Secrets)
	setupInputConfigMaps(pod, &pod.Spec.Containers[0], build.Spec.Source.ConfigMaps)
	setupContainersConfigs(build, pod)
	setupBuildCAs(build, pod, additionalCAs, internalRegistryHost)
	setupContainersStorage(pod, &pod.Spec.Containers[0]) // for unprivileged builds
	// setupContainersNodeStorage(pod, &pod.Spec.Containers[0]) // for privileged builds
	setupBlobCache(pod)
	return pod, nil
}


func (opts *startOptions) createBuildPod(build *buildv1.Build, additionalCAs map[string]string, internalRegistryHost string) (*corev1.Pod, error) {
	var pod *corev1.Pod
	var err error
	switch {
	case build.Spec.Strategy.DockerStrategy != nil:
		pod, err = opts.createDockerBuildPod(build, additionalCAs, internalRegistryHost)
	case build.Spec.Strategy.SourceStrategy != nil:
		pod, err = opts.createSourceBuildPod(build, additionalCAs, internalRegistryHost)
	case build.Spec.Strategy.CustomStrategy != nil:
		pod, err = opts.createCustomBuildPod(build, additionalCAs, internalRegistryHost)
	case build.Spec.Strategy.JenkinsPipelineStrategy != nil:
		return nil, fmt.Errorf("creating a build pod for Build %s/%s with the JenkinsPipeline strategy is not supported", build.Namespace, build.Name)
	default:
		return nil, fmt.Errorf("no supported build strategy defined for Build %s/%s", build.Namespace, build.Name)
	}

	if pod != nil {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[buildv1.BuildAnnotation] = build.Name

		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = map[string]string{}
		}
		//TODO perhaps need k8s bump in tkn cli to get this label
		//pod.Spec.NodeSelector[corev1.LabelOSStable] = "linux"
	}
	return pod, err
}

func GetBuildFromPod(pod *corev1.Pod) (*buildv1.Build, error) {
	if len(pod.Spec.Containers) == 0 {
		return nil, errors.New("unable to get build from pod: pod has no containers")
	}

	buildEnvVar := getEnvVar(&pod.Spec.Containers[0], "BUILD")
	if len(buildEnvVar) == 0 {
		return nil, errors.New("unable to get build from pod: BUILD environment variable is empty")
	}

	obj, err := runtime.Decode(Decoder, []byte(buildEnvVar))
	if err != nil {
		return nil, fmt.Errorf("unable to get build from pod: %v", err)
	}
	build, ok := obj.(*buildv1.Build)
	if !ok {
		return nil, fmt.Errorf("unable to get build from pod: %v", errors.New("decoded object is not of type Build"))
	}
	return build, nil
}

// SetBuildInPod encodes a build object and sets it in a pod's BUILD environment variable
func SetBuildInPod(pod *corev1.Pod, build *buildv1.Build) error {
	encodedBuild, err := runtime.Encode(Encoder, build)
	if err != nil {
		return fmt.Errorf("unable to set build in pod: %v", err)
	}

	for i := range pod.Spec.Containers {
		setEnvVar(&pod.Spec.Containers[i], "BUILD", string(encodedBuild))
	}
	for i := range pod.Spec.InitContainers {
		setEnvVar(&pod.Spec.InitContainers[i], "BUILD", string(encodedBuild))
	}

	return nil
}

func getEnvVar(c *corev1.Container, name string) string {
	for _, envVar := range c.Env {
		if envVar.Name == name {
			return envVar.Value
		}
	}

	return ""
}

func setEnvVar(c *corev1.Container, name, value string) {
	for i, envVar := range c.Env {
		if envVar.Name == name {
			c.Env[i] = corev1.EnvVar{Name: name, Value: value}
			return
		}
	}

	c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
}

func syntaxWrap(input string) string {
	return string(operator) + string(referenceOpener) + input + string(referenceCloser)
}

func MappingFuncFor(context ...map[string]string) func(string) string {
	return func(input string) string {
		for _, vars := range context {
			val, ok := vars[input]
			if ok {
				return val
			}
		}

		return syntaxWrap(input)
	}
}

func Expand(input string, mapping func(string) string) string {
	var buf bytes.Buffer
	checkpoint := 0
	for cursor := 0; cursor < len(input); cursor++ {
		if input[cursor] == operator && cursor+1 < len(input) {
			// Copy the portion of the input string since the last
			// checkpoint into the buffer
			buf.WriteString(input[checkpoint:cursor])

			// Attempt to read the variable name as defined by the
			// syntax from the input string
			read, isVar, advance := tryReadVariableName(input[cursor+1:])

			if isVar {
				// We were able to read a variable name correctly;
				// apply the mapping to the variable name and copy the
				// bytes into the buffer
				buf.WriteString(mapping(read))
			} else {
				// Not a variable name; copy the read bytes into the buffer
				buf.WriteString(read)
			}

			// Advance the cursor in the input string to account for
			// bytes consumed to read the variable name expression
			cursor += advance

			// Advance the checkpoint in the input string
			checkpoint = cursor + 1
		}
	}

	// Return the buffer and any remaining unwritten bytes in the
	// input string.
	return buf.String() + input[checkpoint:]
}


func tryReadVariableName(input string) (string, bool, int) {
	switch input[0] {
	case operator:
		// Escaped operator; return it.
		return input[0:1], false, 1
	case referenceOpener:
		// Scan to expression closer
		for i := 1; i < len(input); i++ {
			if input[i] == referenceCloser {
				return input[1:i], true, i + 1
			}
		}

		// Incomplete reference; return it.
		return string(operator) + string(referenceOpener), false, 1
	default:
		// Not the beginning of an expression, ie, an operator
		// that doesn't begin an expression.  Return the operator
		// and the first rune in the string.
		return (string(operator) + string(input[0])), false, 1
	}
}

type ErrEnvVarResolver struct {
	message kutilerrors.Aggregate
}

// Error returns a string representation of the error
func (e ErrEnvVarResolver) Error() string {
	return fmt.Sprintf("%v", e.message)
}

func (opts *startOptions) resolveValueFrom(pod *corev1.Pod) error {
	var outputEnv []corev1.EnvVar
	var allErrs []error

	client, err := opts.cliparams.KubeClient()
	if err != nil {
		return err
	}

	build, err := GetBuildFromPod(pod)
	if err != nil {
		return nil
	}

	mapEnvs := map[string]string{}
	mapping := MappingFuncFor(mapEnvs)
	inputEnv := sharedbuildutil.GetBuildEnv(build)
	store := envresolve.NewResourceStore()

	for _, e := range inputEnv {
		var value string
		var err error

		if e.Value != "" {
			value = Expand(e.Value, mapping)
		} else if e.ValueFrom != nil {
			value, err = envresolve.GetEnvVarRefValue(client, build.Namespace, store, e.ValueFrom, build, nil)
			if err != nil {
				allErrs = append(allErrs, err)
				continue
			}
		}

		outputEnv = append(outputEnv, corev1.EnvVar{Name: e.Name, Value: value})
		mapEnvs[e.Name] = value
	}

	if len(allErrs) > 0 {
		return ErrEnvVarResolver{kutilerrors.NewAggregate(allErrs)}
	}

	sharedbuildutil.SetBuildEnv(build, outputEnv)
	return SetBuildInPod(pod, build)
}

func (opts *startOptions) createPodSpec(build *buildv1.Build, caData map[string]string) (*corev1.Pod, error) {
	if build.Spec.Output.To != nil {
		build.Status.OutputDockerImageReference = build.Spec.Output.To.Name
	}

	// ensure the build object the pod sees starts with a clean set of reasons/messages,
	// rather than inheriting the potential "invalidoutputreference" message from the current
	// build state.  Otherwise when the pod attempts to update the build (e.g. with the git
	// revision information), it will re-assert the stale reason/message.
	build.Status.Reason = ""
	build.Status.Message = ""

	// Invoke the strategy to create a build pod.
	//TODO perhaps we need to supply the registry hostname as a param to tkn convert
	podSpec, err := opts.createBuildPod(build, caData, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create a build pod spec for build %s/%s: %v", build.Namespace, build.Name, err)
	}
	//TODO should we access build defaults and overrides as part of conversion to tekton


	// Handle resolving ValueFrom references in build environment variables
	if err := opts.resolveValueFrom(podSpec); err != nil {
		return nil, err
	}
	return podSpec, nil
}

func (opts *startOptions) constructBuildPod(b *buildv1.Build) (*corev1.Pod, error) {
	build := b.DeepCopy()
	// Resolve all Docker image references to valid values.
	if err := opts.resolveImageReferences(build); err != nil {
		return nil, err
	}

	// Set the pushSecret that will be needed by the build to push the image to the registry
	// at the end of the build.
	pushSecret := build.Spec.Output.PushSecret
	// Only look up a push secret if the user hasn't explicitly provided one.
	if build.Spec.Output.PushSecret == nil && build.Spec.Output.To != nil && len(build.Spec.Output.To.Name) > 0 {
		var err error
		pushSecret, err = opts.resolveImageSecretAsReference(build, build.Spec.Output.To.Name)
		if err != nil {
			return nil, err
		}
	}
	build.Spec.Output.PushSecret = pushSecret

	build.Spec.Output.PushSecret = pushSecret

	// Set the pullSecret that will be needed by the build to pull the base/builder image.
	var pullSecret *corev1.LocalObjectReference
	var imageName string
	switch {
	case build.Spec.Strategy.SourceStrategy != nil:
		pullSecret = build.Spec.Strategy.SourceStrategy.PullSecret
		imageName = build.Spec.Strategy.SourceStrategy.From.Name
	case build.Spec.Strategy.DockerStrategy != nil:
		pullSecret = build.Spec.Strategy.DockerStrategy.PullSecret
		if build.Spec.Strategy.DockerStrategy.From != nil {
			imageName = build.Spec.Strategy.DockerStrategy.From.Name
		}
	case build.Spec.Strategy.CustomStrategy != nil:
		pullSecret = build.Spec.Strategy.CustomStrategy.PullSecret
		imageName = build.Spec.Strategy.CustomStrategy.From.Name
	}

	// Only look up a pull secret if the user hasn't explicitly provided one
	// if we don't know what image they are referencing, we'll end up using the
	// docker secret associated w/ the build's service account.
	if pullSecret == nil {
		var err error
		pullSecret, err = opts.resolveImageSecretAsReference(build, imageName)
		if err != nil {
			return nil, err
		}
		if pullSecret != nil {
			switch {
			case build.Spec.Strategy.SourceStrategy != nil:
				build.Spec.Strategy.SourceStrategy.PullSecret = pullSecret
			case build.Spec.Strategy.DockerStrategy != nil:
				build.Spec.Strategy.DockerStrategy.PullSecret = pullSecret
			case build.Spec.Strategy.CustomStrategy != nil:
				build.Spec.Strategy.CustomStrategy.PullSecret = pullSecret
			}
		}
	}

	// look up the secrets needed to pull any source input images.
	for i, s := range build.Spec.Source.Images {
		if s.PullSecret != nil {
			continue
		}
		imageInputPullSecret, err := opts.resolveImageSecretAsReference(build, s.From.Name)
		if err != nil {
			return nil, err
		}
		build.Spec.Source.Images[i].PullSecret = imageInputPullSecret
	}

	if build.Spec.Strategy.CustomStrategy != nil {
		updateCustomImageEnv(build.Spec.Strategy.CustomStrategy, build.Spec.Strategy.CustomStrategy.From.Name)
	}

	//TODO do we expose additional trusted CAs from the cluster scoped image config object for conversion
	// to tekton
	// Create the build pod spec
	buildPod, err := opts.createPodSpec(build, map[string]string{})
	if err != nil {
		return nil, err
	}

	return buildPod, nil
}