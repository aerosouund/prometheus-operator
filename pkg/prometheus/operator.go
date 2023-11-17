// Copyright 2016 The prometheus-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus-operator/prometheus-operator/pkg/informers"
	"github.com/prometheus-operator/prometheus-operator/pkg/operator"
)

var prometheusKeyInShardStatefulSet = regexp.MustCompile("^(.+)/prometheus-(.+)-shard-[1-9][0-9]*$")
var prometheusKeyInStatefulSet = regexp.MustCompile("^(.+)/prometheus-(.+)$")

type StatusReporter struct {
	Kclient         kubernetes.Interface
	Reconciliations *operator.ReconciliationTracker
	SsetInfs        *informers.ForResource
	Rr              *operator.ResourceReconciler
}

func StatefulSetKeyToPrometheusKey(key string) (bool, string) {
	r := prometheusKeyInStatefulSet
	if prometheusKeyInShardStatefulSet.MatchString(key) {
		r = prometheusKeyInShardStatefulSet
	}

	matches := r.FindAllStringSubmatch(key, 2)
	if len(matches) != 1 {
		return false, ""
	}
	if len(matches[0]) != 3 {
		return false, ""
	}
	return true, matches[0][1] + "/" + matches[0][2]
}

func KeyToStatefulSetKey(p monitoringv1.PrometheusInterface, key string, shard int) string {
	keyParts := strings.Split(key, "/")
	return fmt.Sprintf("%s/%s", keyParts[0], statefulSetNameFromPrometheusName(p, keyParts[1], shard))
}

func statefulSetNameFromPrometheusName(p monitoringv1.PrometheusInterface, name string, shard int) string {
	if shard == 0 {
		return fmt.Sprintf("%s-%s", prefix(p), name)
	}
	return fmt.Sprintf("%s-%s-shard-%d", prefix(p), name, shard)
}

func NewTLSAssetSecret(p monitoringv1.PrometheusInterface, labels map[string]string) *v1.Secret {
	objMeta := p.GetObjectMeta()
	typeMeta := p.GetTypeMeta()

	boolTrue := true
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:   TLSAssetsSecretName(p),
			Labels: labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         typeMeta.APIVersion,
					BlockOwnerDeletion: &boolTrue,
					Controller:         &boolTrue,
					Kind:               typeMeta.Kind,
					Name:               objMeta.GetName(),
					UID:                objMeta.GetUID(),
				},
			},
		},
		Data: map[string][]byte{},
	}
}

// ValidateRemoteWriteSpec checks that mutually exclusive configurations are not
// included in the Prometheus remoteWrite configuration section, while also validating
// the RemoteWriteSpec child fields.
// Reference:
// https://github.com/prometheus/prometheus/blob/main/docs/configuration/configuration.md#remote_write
func ValidateRemoteWriteSpec(spec monitoringv1.RemoteWriteSpec) error {
	var nonNilFields []string
	for k, v := range map[string]interface{}{
		"basicAuth":     spec.BasicAuth,
		"oauth2":        spec.OAuth2,
		"authorization": spec.Authorization,
		"sigv4":         spec.Sigv4,
		"azureAd":       spec.AzureAD,
	} {
		if reflect.ValueOf(v).IsNil() {
			continue
		}
		nonNilFields = append(nonNilFields, fmt.Sprintf("%q", k))
	}

	if len(nonNilFields) > 1 {
		return fmt.Errorf("%s can't be set at the same time, at most one of them must be defined", strings.Join(nonNilFields, " and "))
	}

	if spec.AzureAD != nil {
		if spec.AzureAD.ManagedIdentity == nil && spec.AzureAD.OAuth == nil {
			return fmt.Errorf("must provide Azure Managed Identity or Azure OAuth in the Azure AD config")
		}

		if spec.AzureAD.ManagedIdentity != nil && spec.AzureAD.OAuth != nil {
			return fmt.Errorf("cannot provide both Azure Managed Identity and Azure OAuth in the Azure AD config")
		}

		if spec.AzureAD.OAuth != nil {
			_, err := uuid.Parse(spec.AzureAD.OAuth.ClientID)
			if err != nil {
				return fmt.Errorf("the provided Azure OAuth clientId is invalid")
			}
		}
	}

	return nil
}

func ValidateAlertmanagerEndpoints(am monitoringv1.AlertmanagerEndpoints) error {
	var nonNilFields []string

	if am.BearerTokenFile != "" {
		nonNilFields = append(nonNilFields, fmt.Sprintf("%q", "bearerTokenFile"))
	}

	for k, v := range map[string]interface{}{
		"basicAuth":     am.BasicAuth,
		"authorization": am.Authorization,
		"sigv4":         am.Sigv4,
	} {
		if reflect.ValueOf(v).IsNil() {
			continue
		}
		nonNilFields = append(nonNilFields, fmt.Sprintf("%q", k))
	}

	if len(nonNilFields) > 1 {
		return fmt.Errorf("%s can't be set at the same time, at most one of them must be defined", strings.Join(nonNilFields, " and "))
	}

	return nil
}

// Process will determine the Status of a Prometheus resource (server or agent) depending on its current state in the cluster
func (sr *StatusReporter) Process(ctx context.Context, p monitoringv1.PrometheusInterface, key string) (*monitoringv1.PrometheusStatus, error) {

	commonFields := p.GetCommonPrometheusFields()
	pStatus := monitoringv1.PrometheusStatus{
		Paused: commonFields.Paused,
	}

	var (
		availableStatus    monitoringv1.ConditionStatus = monitoringv1.ConditionTrue
		availableReason    string
		availableCondition = monitoringv1.Condition{
			Type: monitoringv1.Available,
			LastTransitionTime: metav1.Time{
				Time: time.Now().UTC(),
			},
			ObservedGeneration: p.GetObjectMeta().GetGeneration(),
		}
		messages []string
		replicas = 1
	)

	if commonFields.Replicas != nil {
		replicas = int(*commonFields.Replicas)
	}

	for shard := range ExpectedStatefulSetShardNames(p) {
		ssetName := KeyToStatefulSetKey(p, key, shard)

		obj, err := sr.SsetInfs.Get(ssetName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Statefulset hasn't been created or is already deleted.
				availableStatus = monitoringv1.ConditionFalse
				availableReason = "StatefulSetNotFound"
				messages = append(messages, fmt.Sprintf("shard %d: statefulset %s not found", shard, ssetName))
				pStatus.ShardStatuses = append(
					pStatus.ShardStatuses,
					monitoringv1.ShardStatus{
						ShardID: strconv.Itoa(shard),
					})

				continue
			}

			return nil, fmt.Errorf("failed to retrieve statefulset: %w", err)
		}

		sset := obj.(*appsv1.StatefulSet).DeepCopy()
		if sr.Rr.DeletionInProgress(sset) {
			continue
		}

		stsReporter, err := operator.NewStatefulSetReporter(ctx, sr.Kclient, sset)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve statefulset state: %w", err)
		}

		pStatus.Replicas += int32(len(stsReporter.Pods))
		pStatus.UpdatedReplicas += int32(len(stsReporter.UpdatedPods()))
		pStatus.AvailableReplicas += int32(len(stsReporter.ReadyPods()))
		pStatus.UnavailableReplicas += int32(len(stsReporter.Pods) - len(stsReporter.ReadyPods()))

		pStatus.ShardStatuses = append(
			pStatus.ShardStatuses,
			monitoringv1.ShardStatus{
				ShardID:             strconv.Itoa(shard),
				Replicas:            int32(len(stsReporter.Pods)),
				UpdatedReplicas:     int32(len(stsReporter.UpdatedPods())),
				AvailableReplicas:   int32(len(stsReporter.ReadyPods())),
				UnavailableReplicas: int32(len(stsReporter.Pods) - len(stsReporter.ReadyPods())),
			},
		)

		if len(stsReporter.ReadyPods()) >= replicas {
			// All pods are ready (or the desired number of replicas is zero).
			continue
		}

		switch {
		case len(stsReporter.ReadyPods()) == 0:
			availableReason = "NoPodReady"
			availableStatus = monitoringv1.ConditionFalse
		case availableCondition.Status != monitoringv1.ConditionFalse:
			availableReason = "SomePodsNotReady"
			availableStatus = monitoringv1.ConditionDegraded
		}

		for _, p := range stsReporter.Pods {
			if m := p.Message(); m != "" {
				messages = append(messages, fmt.Sprintf("shard %d: pod %s: %s", shard, p.Name, m))
			}
		}
	}

	pStatus.Conditions = operator.UpdateConditions(
		pStatus.Conditions,
		monitoringv1.Condition{
			Type:    monitoringv1.Available,
			Status:  availableStatus,
			Reason:  availableReason,
			Message: strings.Join(messages, "\n"),
			LastTransitionTime: metav1.Time{
				Time: time.Now().UTC(),
			},
			ObservedGeneration: p.GetObjectMeta().GetGeneration(),
		},
		sr.Reconciliations.GetCondition(key, p.GetObjectMeta().GetGeneration()),
	)

	return &pStatus, nil
}
