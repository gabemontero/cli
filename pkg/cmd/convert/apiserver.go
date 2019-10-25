package convert

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/credentialprovider"
	credentialprovidersecrets "k8s.io/kubernetes/pkg/credentialprovider/secrets"

	buildv1 "github.com/openshift/api/build/v1"
	"github.com/openshift/library-go/pkg/build/buildutil"
	imagev1 "github.com/openshift/api/image/v1"
	"github.com/openshift/library-go/pkg/image/imageutil")

func dockerImageReferenceNameString(r imagev1.DockerImageReference) string {
	switch {
	case len(r.Name) == 0:
		return ""
	case len(r.Tag) > 0:
		return r.Name + ":" + r.Tag
	case len(r.ID) > 0:
		var ref string
		if _, err := imageutil.ParseDigest(r.ID); err == nil {
			// if it parses as a digest, its v2 pull by id
			ref = "@" + r.ID
		} else {
			// if it doesn't parse as a digest, it's presumably a v1 registry by-id tag
			ref = ":" + r.ID
		}
		return r.Name + ref
	default:
		return r.Name
	}
}

func dockerImageReferenceExact(r imagev1.DockerImageReference) string {
	name := dockerImageReferenceNameString(r)
	if len(name) == 0 {
		return name
	}
	s := r.Registry
	if len(s) > 0 {
		s += "/"
	}
	if len(r.Namespace) != 0 {
		s += r.Namespace + "/"
	}
	return s + name
}

func latestImageTagEvent(stream *imagev1.ImageStream, imageID string) (string, *imagev1.TagEvent) {
	var (
		latestTagEvent *imagev1.TagEvent
		latestTag      string
	)
	for _, events := range stream.Status.Tags {
		if len(events.Items) == 0 {
			continue
		}
		tag := events.Tag
		for i, event := range events.Items {
			if imageutil.DigestOrImageMatch(event.Image, imageID) &&
				(latestTagEvent == nil || latestTagEvent != nil && event.Created.After(latestTagEvent.Created.Time)) {
				latestTagEvent = &events.Items[i]
				latestTag = tag
			}
		}
	}
	return latestTag, latestTagEvent
}

func dockerImageReferenceForImage(stream *imagev1.ImageStream, imageID string) (string, bool) {
	tag, event := latestImageTagEvent(stream, imageID)
	if len(tag) == 0 {
		return "", false
	}
	var ref *imagev1.TagReference
	for _, t := range stream.Spec.Tags {
		if t.Name == tag {
			ref = &t
			break
		}
	}
	if ref == nil {
		return event.DockerImageReference, true
	}
	switch ref.ReferencePolicy.Type {
	case imagev1.LocalTagReferencePolicy:
		ref, err := imageutil.ParseDockerImageReference(stream.Status.DockerImageRepository)
		if err != nil {
			return event.DockerImageReference, true
		}
		ref.Tag = ""
		ref.ID = event.Image
		return dockerImageReferenceExact(ref), true
	default:
		return event.DockerImageReference, true
	}
}

func (opt *startOptions) resolveImageStreamReference(from corev1.ObjectReference, defaultNamespace string) (string, error) {
	var namespace string
	if len(from.Namespace) != 0 {
		namespace = from.Namespace
	} else {
		namespace = defaultNamespace
	}

	clients, err := opt.cliparams.Clients()
	if err != nil {
		return "", err
	}
	klog.V(4).Infof("Resolving ImageStreamReference %s of Kind %s in namespace %s", from.Name, from.Kind, namespace)
	switch from.Kind {
	case "ImageStreamImage":
		name, id, err := imageutil.ParseImageStreamImageName(from.Name)
		if err != nil {
			return "", err
		}

		stream, err := clients.OpenShiftImage.ImageStreams(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		reference, ok := dockerImageReferenceForImage(stream, id)
		if !ok {
			return "", err
		}
		klog.V(4).Infof("Resolved ImageStreamImage %s to image %q", from.Name, reference)
		return reference, nil

	case "ImageStreamTag":
		name, tag, err := imageutil.ParseImageStreamTagName(from.Name)
		if err != nil {
			return "", err
		}
		stream, err := clients.OpenShiftImage.ImageStreams(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		reference, ok := imageutil.ResolveLatestTaggedImage(stream, tag)
		if !ok {
			return "", err
		}
		klog.V(4).Infof("Resolved ImageStreamTag %s to image %q", from.Name, reference)
		return reference, nil
	case "DockerImage":
		return from.Name, nil
	default:
		return "", fmt.Errorf("unknown From Kind %s", from.Kind)
	}
}

func labelValue(name string) string {
	if len(name) <= validation.DNS1123LabelMaxLength {
		return name
	}
	return name[:validation.DNS1123LabelMaxLength]
}

func dockerImageReferenceForStream(stream *imagev1.ImageStream) (imagev1.DockerImageReference, error) {
	spec := stream.Status.DockerImageRepository
	if len(spec) == 0 {
		spec = stream.Spec.DockerImageRepository
	}
	if len(spec) == 0 {
		return imagev1.DockerImageReference{}, fmt.Errorf("no possible pull spec for %s/%s", stream.Namespace, stream.Name)
	}
	return imageutil.ParseDockerImageReference(spec)
}

func (opt *startOptions) resolveImageStreamDockerRepository(from corev1.ObjectReference, defaultNamespace string) (string, error) {
	namespace := defaultNamespace
	if len(from.Namespace) > 0 {
		namespace = from.Namespace
	}
	clients, err := opt.cliparams.Clients()
	if err != nil {
		return "", err
	}
	klog.V(4).Infof("Resolving ImageStreamReference %s of Kind %s in namespace %s", from.Name, from.Kind, namespace)
	switch from.Kind {
	case "ImageStreamImage":
		imageStreamImage, err := clients.OpenShiftImage.ImageStreamImages(namespace).Get(from.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		image := imageStreamImage.Image
		klog.V(4).Infof("Resolved ImageStreamReference %s to image %s with reference %s in namespace %s", from.Name, image.Name, image.DockerImageReference, namespace)
		return image.DockerImageReference, nil
	case "ImageStreamTag":
		name := strings.Split(from.Name, ":")[0]
		is , err := clients.OpenShiftImage.ImageStreams(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		image, err := dockerImageReferenceForStream(is)
		if err != nil {
			klog.V(2).Infof("Error resolving container image reference for %s/%s: %v", namespace, name, err)
			return "", err
		}
		klog.V(4).Infof("Resolved ImageStreamTag %s/%s to repository %s", namespace, from.Name, image)
		return image.String(), nil
	case "DockerImage":
		return from.Name, nil
	default:
		return "", fmt.Errorf("unknown From Kind %s", from.Kind)
	}
}

func findDockerSecretAsReference(secrets []corev1.Secret, image string) *corev1.LocalObjectReference {
	emptyKeyring := credentialprovider.BasicDockerKeyring{}
	for _, secret := range secrets {
		secretList := []corev1.Secret{*secret.DeepCopy()}
		keyring, err := credentialprovidersecrets.MakeDockerKeyring(secretList, &emptyKeyring)
		if err != nil {
			klog.V(2).Infof("Unable to make the Docker keyring for %s/%s secret: %v", secret.Name, secret.Namespace, err)
			continue
		}
		if _, found := keyring.Lookup(image); found {
			return &corev1.LocalObjectReference{Name: secret.Name}
		}
	}
	return nil
}

func getImageChangeTriggerForRef(bc *buildv1.BuildConfig, ref *corev1.ObjectReference) *buildv1.ImageChangeTrigger {
	if ref == nil || ref.Kind != "ImageStreamTag" {
		return nil
	}
	for _, trigger := range bc.Spec.Triggers {
		if trigger.Type == buildv1.ImageChangeBuildTriggerType && trigger.ImageChange.From != nil &&
			trigger.ImageChange.From.Name == ref.Name && trigger.ImageChange.From.Namespace == ref.Namespace {
			return trigger.ImageChange
		}
	}
	return nil
}

func (opt *startOptions) resolveImageSecret(secrets []corev1.Secret, imageRef *corev1.ObjectReference, buildNamespace string) *corev1.LocalObjectReference {
	if len(secrets) == 0 || imageRef == nil {
		return nil
	}
	// Get the image pull spec from the image stream reference
	imageSpec, err := opt.resolveImageStreamDockerRepository(*imageRef, buildNamespace)
	if err != nil {
		klog.V(2).Infof("Unable to resolve the image name for %s/%s: %v", buildNamespace, imageRef, err)
		return nil
	}
	s := findDockerSecretAsReference(secrets, imageSpec)
	if s == nil {
		klog.V(4).Infof("No secrets found for pushing or pulling the %s  %s/%s", imageRef.Kind, buildNamespace, imageRef.Name)
	}
	return s
}

func updateCustomImageEnv(strategy *buildv1.CustomBuildStrategy, newImage string) {
	if strategy.Env == nil {
		strategy.Env = make([]corev1.EnvVar, 1)
		strategy.Env[0] = corev1.EnvVar{Name: buildv1.CustomBuildStrategyBaseImageKey, Value: newImage}
	} else {
		found := false
		for i := range strategy.Env {
			klog.V(4).Infof("Checking env variable %s %s", strategy.Env[i].Name, strategy.Env[i].Value)
			if strategy.Env[i].Name == buildv1.CustomBuildStrategyBaseImageKey {
				found = true
				strategy.Env[i].Value = newImage
				klog.V(4).Infof("Updated env variable %s to %s", strategy.Env[i].Name, strategy.Env[i].Value)
				break
			}
		}
		if !found {
			strategy.Env = append(strategy.Env, corev1.EnvVar{Name: buildv1.CustomBuildStrategyBaseImageKey, Value: newImage})
		}
	}
}

func (opt *startOptions) constructBuildObject(bc *buildv1.BuildConfig, request *buildv1.BuildRequest, buildName, namespace string) (*buildv1.Build, error) {
	bcCopy := bc.DeepCopy()
	serviceAccount := bcCopy.Spec.ServiceAccount
	if len(serviceAccount) == 0 {
		serviceAccount = "builder" //bootstrappolicy.BuilderServiceAccountName
	}
	t := true
	build := &buildv1.Build{
		Spec: buildv1.BuildSpec{
			CommonSpec: buildv1.CommonSpec{
				ServiceAccount:            serviceAccount,
				Source:                    bcCopy.Spec.Source,
				Strategy:                  bcCopy.Spec.Strategy,
				Output:                    bcCopy.Spec.Output,
				Revision:                  request.Revision,
				Resources:                 bcCopy.Spec.Resources,
				PostCommit:                bcCopy.Spec.PostCommit,
				CompletionDeadlineSeconds: bcCopy.Spec.CompletionDeadlineSeconds,
				NodeSelector:              bcCopy.Spec.NodeSelector,
			},
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   buildName,
			Namespace: namespace,
			Labels: bcCopy.Labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: buildv1.GroupVersion.String(), // BuildConfig.APIVersion is not populated
					Kind:       "BuildConfig",                 // BuildConfig.Kind is not populated
					Name:       bcCopy.Name,
					UID:        bcCopy.UID,
					Controller: &t,
				},
			},
		},
		Status: buildv1.BuildStatus{
			Phase: buildv1.BuildPhaseNew,
			Config: &corev1.ObjectReference{
				Kind:      "BuildConfig",
				Name:      bcCopy.Name,
				Namespace: bcCopy.Namespace,
			},
		},
	}

	build.Spec.Source.Type = ""
	build.Spec.Source.Binary = nil
	if build.Annotations == nil {
		build.Annotations = make(map[string]string)
	}
	// bcCopy.Status.LastVersion has been increased
	build.Annotations[buildv1.BuildNumberAnnotation] = strconv.FormatInt(bcCopy.Status.LastVersion, 10)
	build.Annotations[buildv1.BuildConfigAnnotation] = bcCopy.Name
	if build.Labels == nil {
		build.Labels = make(map[string]string)
	}
	build.Labels[buildv1.BuildConfigLabelDeprecated] = labelValue(bcCopy.Name)
	build.Labels[buildv1.BuildConfigLabel] = labelValue(bcCopy.Name)
	build.Labels[buildv1.BuildRunPolicyLabel] = string(bcCopy.Spec.RunPolicy)


	kubeClient, err := opt.cliparams.KubeClient()
	if err != nil {
		return nil, err
	}
	sa, err := kubeClient.CoreV1().ServiceAccounts(namespace).Get(serviceAccount, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var builderSecrets []corev1.Secret
	for _, ref := range sa.Secrets {
		secret, err := kubeClient.CoreV1().Secrets(namespace).Get(ref.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		builderSecrets = append(builderSecrets, *secret)
	}
	// Resolve image source if present
	var strategyImageChangeTrigger *buildv1.ImageChangeTrigger
	for _, trigger := range bc.Spec.Triggers {
		if trigger.Type == buildv1.ImageChangeBuildTriggerType && trigger.ImageChange.From == nil {
			strategyImageChangeTrigger = trigger.ImageChange
			break
		}
	}
	for i, sourceImage := range build.Spec.Source.Images {
		if sourceImage.PullSecret == nil {

			sourceImage.PullSecret = opt.resolveImageSecret(builderSecrets, &sourceImage.From, namespace)
			if sourceImage.PullSecret == nil {
				klog.V(4).Infof("No secrets found for pushing or pulling the %s  %s/%s", sourceImage.From.Kind, namespace, sourceImage.From.Name)
			}

		}

		var sourceImageSpec string
		// if the imagesource matches the strategy from, and we have a trigger for the strategy from,
		// use the imageid from the trigger rather than resolving it.
		if strategyFrom := buildutil.GetInputReference(bcCopy.Spec.Strategy); strategyFrom != nil &&
			reflect.DeepEqual(sourceImage.From, *strategyFrom) &&
			strategyImageChangeTrigger != nil {
			sourceImageSpec = strategyImageChangeTrigger.LastTriggeredImageID
		} else {
			refImageChangeTrigger := getImageChangeTriggerForRef(bcCopy, &sourceImage.From)
			// if there is no trigger associated with this imagesource, resolve the imagesource reference now.
			// otherwise use the imageid from the imagesource trigger.
			if refImageChangeTrigger == nil {
				sourceImageSpec, err = opt.resolveImageStreamReference(sourceImage.From, bcCopy.Namespace)
				if err != nil {
					return nil, err
				}
			} else {
				sourceImageSpec = refImageChangeTrigger.LastTriggeredImageID
			}
		}

		sourceImage.From.Kind = "DockerImage"
		sourceImage.From.Name = sourceImageSpec
		sourceImage.From.Namespace = ""
		build.Spec.Source.Images[i] = sourceImage
	}

	var image string

	if strategyImageChangeTrigger != nil {
		image = strategyImageChangeTrigger.LastTriggeredImageID
	}
	// If the Build is using a From reference instead of a resolved image, we need to resolve that From
	// reference to a valid image so we can run the build.  Builds do not consume ImageStream references,
	// only image specs.
	switch {
	case build.Spec.Strategy.SourceStrategy != nil:
		if image == "" {
			image, err = opt.resolveImageStreamReference(build.Spec.Strategy.SourceStrategy.From, bcCopy.Namespace)
			if err != nil {
				return nil, err
			}
		}
		build.Spec.Strategy.SourceStrategy.From = corev1.ObjectReference{
			Kind: "DockerImage",
			Name: image,
		}
		if build.Spec.Strategy.SourceStrategy.PullSecret == nil {
			build.Spec.Strategy.SourceStrategy.PullSecret = opt.resolveImageSecret(builderSecrets, &build.Spec.Strategy.SourceStrategy.From, bcCopy.Namespace)
		}
	case build.Spec.Strategy.DockerStrategy != nil &&
		build.Spec.Strategy.DockerStrategy.From != nil:
		if image == "" {
			image, err = opt.resolveImageStreamReference(*build.Spec.Strategy.DockerStrategy.From, bcCopy.Namespace)
			if err != nil {
				return nil, err
			}
		}
		build.Spec.Strategy.DockerStrategy.From = &corev1.ObjectReference{
			Kind: "DockerImage",
			Name: image,
		}
		if build.Spec.Strategy.DockerStrategy.PullSecret == nil {
			build.Spec.Strategy.DockerStrategy.PullSecret = opt.resolveImageSecret(builderSecrets, build.Spec.Strategy.DockerStrategy.From, bcCopy.Namespace)
		}
	case build.Spec.Strategy.CustomStrategy != nil:
		if image == "" {
			image, err = opt.resolveImageStreamReference(build.Spec.Strategy.CustomStrategy.From, bcCopy.Namespace)
			if err != nil {
				return nil, err
			}
		}
		build.Spec.Strategy.CustomStrategy.From = corev1.ObjectReference{
			Kind: "DockerImage",
			Name: image,
		}
		if build.Spec.Strategy.CustomStrategy.PullSecret == nil {
			build.Spec.Strategy.CustomStrategy.PullSecret = opt.resolveImageSecret(builderSecrets, &build.Spec.Strategy.CustomStrategy.From, bcCopy.Namespace)
		}
		updateCustomImageEnv(build.Spec.Strategy.CustomStrategy, image)
	}
	return build, nil
}
