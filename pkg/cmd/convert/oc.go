package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	buildv1 "github.com/openshift/api/build/v1"
)

func constructBuildRequest(buildName, namespace string) *buildv1.BuildRequest {
	buildRequestCauses := []buildv1.BuildTriggerCause{}
	request := &buildv1.BuildRequest{
		TriggeredBy: append(buildRequestCauses,
			buildv1.BuildTriggerCause{
				Message: "Manually triggered",
			},
		),
		ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace},
	}

	request.SourceStrategyOptions = &buildv1.SourceStrategyOptions{}
	request.DockerStrategyOptions = &buildv1.DockerStrategyOptions{}
	return request
}