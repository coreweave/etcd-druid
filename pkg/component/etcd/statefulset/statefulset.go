// Copyright (c) 2022 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statefulset

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/etcd-druid/pkg/utils"

	gardenercomponent "github.com/gardener/gardener/pkg/operation/botanist/component"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/retry"
	gardenerretry "github.com/gardener/gardener/pkg/utils/retry"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Interface contains functions for a StatefulSet deployer.
type Interface interface {
	gardenercomponent.DeployWaiter
	// Get gets the etcd StatefulSet.
	Get(context.Context) (*appsv1.StatefulSet, error)
}

type component struct {
	client client.Client
	logger logr.Logger

	values Values
}

func (c *component) Get(ctx context.Context) (*appsv1.StatefulSet, error) {
	sts := c.emptyStatefulset()

	if err := c.client.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		return nil, err
	}

	return sts, nil
}

func (c *component) Deploy(ctx context.Context) error {
	sts, err := c.Get(ctx)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		sts = c.emptyStatefulset()
	}

	if sts.Generation > 1 && sts.Spec.ServiceName != c.values.ServiceName {
		// Earlier clusters referred to the client service in `sts.Spec.ServiceName` which must be changed
		// when a multi-node cluster is used, see https://github.com/gardener/etcd-druid/pull/293.
		if clusterScaledUpToMultiNode(c.values) {
			deleteAndWait := gardenercomponent.OpDestroyAndWait(c)
			if err := deleteAndWait.Destroy(ctx); err != nil {
				return err
			}
			sts = c.emptyStatefulset()
		}
	}

	return c.syncStatefulset(ctx, sts)
}

func (c *component) Destroy(ctx context.Context) error {
	sts := c.emptyStatefulset()

	if err := c.deleteStatefulset(ctx, sts); err != nil {
		return err
	}
	return nil
}

func clusterScaledUpToMultiNode(val Values) bool {
	return val.Replicas > 1 &&
		// Also consider `0` here because this field was not maintained in earlier releases.
		(val.StatusReplicas == 0 ||
			val.StatusReplicas == 1)
}

const (
	// defaultInterval is the default interval for retry operations.
	defaultInterval = 5 * time.Second
	// defaultTimeout is the default timeout for retry operations.
	defaultTimeout = 90 * time.Second
)

func (c *component) Wait(ctx context.Context) error {
	sts := c.emptyStatefulset()

	err := gardenerretry.UntilTimeout(ctx, defaultInterval, defaultTimeout, func(ctx context.Context) (bool, error) {
		if err := c.client.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
			if apierrors.IsNotFound(err) {
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}
		if err := utils.CheckStatefulSet(c.values.Replicas, sts); err != nil {
			return gardenerretry.MinorError(err)
		}
		return gardenerretry.Ok()
	})
	if err != nil {
		messages, err2 := c.fetchPVCEventsFor(ctx, sts)
		if err2 != nil {
			c.logger.Error(err2, "Error while fetching events for depending PVC")
			// don't expose this error since fetching events is a best effort
			// and shouldn't be confused with the actual error
			return err
		}
		if messages != "" {
			return fmt.Errorf("%w\n\n%s", err, messages)
		}
	}

	return err
}

func (c *component) WaitCleanup(ctx context.Context) error {
	return gardenerretry.UntilTimeout(ctx, defaultInterval, defaultTimeout, func(ctx context.Context) (done bool, err error) {
		sts := c.emptyStatefulset()
		err = c.client.Get(ctx, client.ObjectKeyFromObject(sts), sts)
		switch {
		case apierrors.IsNotFound(err):
			return retry.Ok()
		case err == nil:
			// StatefulSet is still available, so we should retry.
			return false, nil
		default:
			return retry.SevereError(err)
		}
	})
}

func (c *component) syncStatefulset(ctx context.Context, sts *appsv1.StatefulSet) error {
	var (
		stsOriginal = sts.DeepCopy()
		patch       = client.StrategicMergeFrom(stsOriginal)
	)

	sts.ObjectMeta = getObjectMeta(&c.values)
	sts.Spec = appsv1.StatefulSetSpec{
		PodManagementPolicy: appsv1.ParallelPodManagement,
		UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
			Type: appsv1.RollingUpdateStatefulSetStrategyType,
		},
		Replicas:    pointer.Int32(c.values.Replicas),
		ServiceName: c.values.ServiceName,
		Selector: &metav1.LabelSelector{
			MatchLabels: getCommonLabels(&c.values),
		},
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: c.values.Annotations,
				Labels:      sts.GetLabels(),
			},
			Spec: v1.PodSpec{
				HostAliases: []v1.HostAlias{
					{
						IP:        "127.0.0.1",
						Hostnames: []string{c.values.Name + "-local"},
					},
				},
				ServiceAccountName:        c.values.ServiceAccountName,
				Affinity:                  c.values.Affinity,
				TopologySpreadConstraints: c.values.TopologySpreadConstraints,
				Containers: []v1.Container{
					{
						Name:            "etcd",
						Image:           c.values.EtcdImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Command:         c.values.EtcdCommand,
						ReadinessProbe: &v1.Probe{
							Handler: v1.Handler{
								Exec: &v1.ExecAction{
									Command: c.values.ReadinessProbeCommand,
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       5,
							FailureThreshold:    5,
						},
						LivenessProbe: &v1.Probe{
							Handler: v1.Handler{
								Exec: &v1.ExecAction{
									Command: c.values.LivenessProbCommand,
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       5,
							FailureThreshold:    5,
						},
						Ports:        getEtcdPorts(c.values),
						Resources:    getEtcdResources(c.values),
						Env:          getEtcdEnvVars(c.values),
						VolumeMounts: getEtcdVolumeMounts(c.values),
					},
					{
						Name:            "backup-restore",
						Image:           c.values.BackupImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Command:         c.values.EtcdBackupCommand,
						Ports:           getBackupPorts(c.values),
						Resources:       getBackupResources(c.values),
						Env:             getBackupRestoreEnvVars(c.values),
						VolumeMounts:    getBackupRestoreVolumeMounts(c.values),
						SecurityContext: &v1.SecurityContext{
							Capabilities: &v1.Capabilities{
								Add: []v1.Capability{
									"SYS_PTRACE",
								},
							},
						},
					},
				},
				ShareProcessNamespace: pointer.Bool(true),
				Volumes:               getVolumes(c.values),
			},
		},
		VolumeClaimTemplates: []v1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: c.values.VolumeClaimTemplateName,
				},
				Spec: v1.PersistentVolumeClaimSpec{
					AccessModes: []v1.PersistentVolumeAccessMode{
						v1.ReadWriteOnce,
					},
					StorageClassName: c.values.StorageClass,
					Resources:        getStorageReq(c.values),
				},
			},
		},
	}
	if c.values.PriorityClassName != nil {
		sts.Spec.Template.Spec.PriorityClassName = *c.values.PriorityClassName
	}

	if stsOriginal.Generation > 0 {
		return c.client.Patch(ctx, sts, patch)
	}

	return c.client.Create(ctx, sts)
}

func (c *component) deleteStatefulset(ctx context.Context, sts *appsv1.StatefulSet) error {
	return client.IgnoreNotFound(c.client.Delete(ctx, sts))
}

func (c *component) fetchPVCEventsFor(ctx context.Context, ss *appsv1.StatefulSet) (string, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.client.List(ctx, pvcs, client.InNamespace(ss.GetNamespace())); err != nil {
		return "", err
	}

	var (
		pvcMessages  string
		volumeClaims = ss.Spec.VolumeClaimTemplates
	)
	for _, volumeClaim := range volumeClaims {
		for _, pvc := range pvcs.Items {
			if !strings.HasPrefix(pvc.GetName(), fmt.Sprintf("%s-%s", volumeClaim.Name, ss.Name)) || pvc.Status.Phase == corev1.ClaimBound {
				continue
			}
			messages, err := kutil.FetchEventMessages(ctx, c.client.Scheme(), c.client, &pvc, corev1.EventTypeWarning, 2)
			if err != nil {
				return "", err
			}
			if messages != "" {
				pvcMessages += fmt.Sprintf("Warning for PVC %s:\n%s\n", pvc.Name, messages)
			}
		}
	}
	return pvcMessages, nil
}

// New creates a new statefulset deployer instance.
func New(c client.Client, logger logr.Logger, values Values) Interface {
	objectLogger := logger.WithValues("sts", client.ObjectKey{Name: values.Name, Namespace: values.Namespace})

	return &component{
		client: c,
		logger: objectLogger,
		values: values,
	}
}

func (c *component) emptyStatefulset() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.values.Name,
			Namespace: c.values.Namespace,
		},
	}
}

func getCommonLabels(val *Values) map[string]string {
	return map[string]string{
		"name":     "etcd",
		"instance": val.Name,
	}
}

func getObjectMeta(val *Values) metav1.ObjectMeta {
	labels := utils.MergeStringMaps(getCommonLabels(val), val.Labels)

	annotations := utils.MergeStringMaps(
		map[string]string{
			"gardener.cloud/owned-by":   fmt.Sprintf("%s/%s", val.Namespace, val.Name),
			"gardener.cloud/owner-type": "etcd",
		},
		val.Annotations,
	)

	ownerRefs := []metav1.OwnerReference{
		{
			APIVersion:         druidv1alpha1.GroupVersion.String(),
			Kind:               "Etcd",
			Name:               val.Name,
			UID:                val.EtcdUID,
			Controller:         pointer.BoolPtr(true),
			BlockOwnerDeletion: pointer.BoolPtr(true),
		},
	}

	return metav1.ObjectMeta{
		Name:            val.Name,
		Namespace:       val.Namespace,
		Labels:          labels,
		Annotations:     annotations,
		OwnerReferences: ownerRefs,
	}
}

func getEtcdPorts(val Values) []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{
			Name:          "server",
			Protocol:      "TCP",
			ContainerPort: pointer.Int32Deref(val.ServerPort, defaultServerPort),
		},
		{
			Name:          "client",
			Protocol:      "TCP",
			ContainerPort: pointer.Int32Deref(val.ClientPort, defaultClientPort),
		},
	}
}

var defaultResourceRequirements = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	},
}

func getEtcdResources(val Values) corev1.ResourceRequirements {
	if val.EtcdResourceRequirements != nil {
		return *val.EtcdResourceRequirements
	}

	return defaultResourceRequirements
}

func getEtcdEnvVars(val Values) []corev1.EnvVar {
	var env []corev1.EnvVar
	env = append(env, getEnvVarFromValue("ENABLE_TLS", strconv.FormatBool(val.BackupTLS != nil)))

	protocol := "http"
	if val.BackupTLS != nil {
		protocol = "https"
	}

	endpoint := fmt.Sprintf("%s://%s-local:%d", protocol, val.Name, pointer.Int32Deref(val.BackupPort, defaultBackupPort))
	env = append(env, getEnvVarFromValue("BACKUP_ENDPOINT", endpoint))

	// This env var has been unused for a long time but is kept to not unnecessarily restart etcds.
	// Todo(timuthy): Remove this as part of a future release in which an etcd restart is acceptable.
	env = append(env, getEnvVarFromValue("FAIL_BELOW_REVISION_PARAMETER", ""))

	return env
}

func getEtcdVolumeMounts(val Values) []corev1.VolumeMount {
	vms := []corev1.VolumeMount{
		{
			Name:      val.VolumeClaimTemplateName,
			MountPath: "/var/etcd/data/",
		},
	}

	vms = append(vms, getSecretVolumeMounts(val.ClientUrlTLS, val.PeerUrlTLS)...)

	return vms
}

func getSecretVolumeMounts(clientUrlTLS, peerUrlTLS *druidv1alpha1.TLSConfig) []corev1.VolumeMount {
	var vms []corev1.VolumeMount

	if clientUrlTLS != nil {
		vms = append(vms, corev1.VolumeMount{
			Name:      "client-url-ca-etcd",
			MountPath: "/var/etcd/ssl/client/ca",
		}, corev1.VolumeMount{
			Name:      "client-url-etcd-server-tls",
			MountPath: "/var/etcd/ssl/client/server",
		}, corev1.VolumeMount{
			Name:      "client-url-etcd-client-tls",
			MountPath: "/var/etcd/ssl/client/client",
		})
	}

	if peerUrlTLS != nil {
		vms = append(vms, corev1.VolumeMount{
			Name:      "peer-url-ca-etcd",
			MountPath: "/var/etcd/ssl/peer/ca",
		}, corev1.VolumeMount{
			Name:      "peer-url-etcd-server-tls",
			MountPath: "/var/etcd/ssl/peer/server",
		})
	}

	return vms
}

func getBackupRestoreVolumeMounts(val Values) []corev1.VolumeMount {
	vms := []corev1.VolumeMount{
		{
			Name:      val.VolumeClaimTemplateName,
			MountPath: "/var/etcd/data",
		},
		{
			Name:      "etcd-config-file",
			MountPath: "/var/etcd/config/",
		},
	}

	vms = append(vms, getSecretVolumeMounts(val.ClientUrlTLS, val.PeerUrlTLS)...)

	if val.BackupStore == nil {
		return vms
	}

	provider, err := utils.StorageProviderFromInfraProvider(val.BackupStore.Provider)
	if err != nil {
		return vms
	}

	switch provider {
	case utils.Local:
		if val.BackupStore.Container != nil {
			vms = append(vms, corev1.VolumeMount{
				Name:      "host-storage",
				MountPath: *val.BackupStore.Container,
			})
		}
	case utils.GCS:
		vms = append(vms, corev1.VolumeMount{
			Name:      "etcd-backup",
			MountPath: "/root/.gcp/",
		})
	case utils.S3, utils.ABS, utils.OSS, utils.Swift, utils.OCS:
		vms = append(vms, corev1.VolumeMount{
			Name:      "etcd-backup",
			MountPath: "/root/etcd-backup/",
		})
	}

	return vms
}

func getStorageReq(val Values) corev1.ResourceRequirements {
	storageCapacity := defaultStorageCapacity
	if val.StorageCapacity != nil {
		storageCapacity = *val.StorageCapacity
	}

	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceStorage: storageCapacity,
		},
	}
}

func getBackupPorts(val Values) []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{
			Name:          "server",
			Protocol:      "TCP",
			ContainerPort: pointer.Int32Deref(val.BackupPort, defaultBackupPort),
		},
	}
}

func getBackupResources(val Values) corev1.ResourceRequirements {
	if val.BackupResourceRequirements != nil {
		return *val.BackupResourceRequirements
	}
	return defaultResourceRequirements
}

func getVolumes(val Values) []corev1.Volume {
	vs := []corev1.Volume{
		{
			Name: "etcd-config-file",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: val.ConfigMapName,
					},
					Items: []corev1.KeyToPath{
						{
							Key:  "etcd.conf.yaml",
							Path: "etcd.conf.yaml",
						},
					},
					DefaultMode: pointer.Int32(0644),
				},
			},
		},
	}

	if val.ClientUrlTLS != nil {
		vs = append(vs, corev1.Volume{
			Name: "client-url-ca-etcd",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: val.ClientUrlTLS.TLSCASecretRef.Name,
				},
			},
		},
			corev1.Volume{
				Name: "client-url-etcd-server-tls",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: val.ClientUrlTLS.ServerTLSSecretRef.Name,
					},
				},
			},
			corev1.Volume{
				Name: "client-url-etcd-client-tls",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: val.ClientUrlTLS.ClientTLSSecretRef.Name,
					},
				},
			})
	}

	if val.PeerUrlTLS != nil {
		vs = append(vs, corev1.Volume{
			Name: "peer-url-ca-etcd",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: val.PeerUrlTLS.TLSCASecretRef.Name,
				},
			},
		},
			corev1.Volume{
				Name: "peer-url-etcd-server-tls",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: val.PeerUrlTLS.ServerTLSSecretRef.Name,
					},
				},
			})
	}

	if val.BackupStore == nil {
		return vs
	}

	storeValues := val.BackupStore
	provider, err := utils.StorageProviderFromInfraProvider(storeValues.Provider)
	if err != nil {
		return vs
	}

	switch provider {
	case "Local":
		hpt := corev1.HostPathDirectory
		vs = append(vs, corev1.Volume{
			Name: "host-storage",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: defaultLocalPrefix + "/" + *storeValues.Container,
					Type: &hpt,
				},
			},
		})
	case utils.GCS, utils.S3, utils.OSS, utils.ABS, utils.Swift, utils.OCS:
		vs = append(vs, corev1.Volume{
			Name: "etcd-backup",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: storeValues.SecretRef.Name,
				},
			},
		})
	}

	return vs
}

func getBackupRestoreEnvVars(val Values) []corev1.EnvVar {
	var (
		env              []corev1.EnvVar
		storageContainer string
		storeValues      = val.BackupStore
	)

	if val.BackupStore != nil {
		storageContainer = pointer.StringDeref(val.BackupStore.Container, "")
	}

	// TODO(timuthy): Move STORAGE_CONTAINER a few lines below so that we can append and exit in one step. This should only be done in a release where a restart of etcd is acceptable.
	env = append(env, getEnvVarFromValue("STORAGE_CONTAINER", storageContainer))
	env = append(env, getEnvVarFromField("POD_NAME", "metadata.name"))
	env = append(env, getEnvVarFromField("POD_NAMESPACE", "metadata.namespace"))

	if storeValues == nil {
		return env
	}

	provider, err := utils.StorageProviderFromInfraProvider(val.BackupStore.Provider)
	if err != nil {
		return env
	}

	// TODO(timuthy): move this to a non root path when we switch to a rootless distribution
	const credentialsMountPath = "/root/etcd-backup"
	switch provider {
	case utils.S3:
		env = append(env, getEnvVarFromValue("AWS_APPLICATION_CREDENTIALS", credentialsMountPath))

	case utils.ABS:
		env = append(env, getEnvVarFromValue("AZURE_APPLICATION_CREDENTIALS", credentialsMountPath))

	case utils.GCS:
		env = append(env, getEnvVarFromValue("GOOGLE_APPLICATION_CREDENTIALS", "/root/.gcp/serviceaccount.json"))

	case utils.Swift:
		env = append(env, getEnvVarFromValue("OPENSTACK_APPLICATION_CREDENTIALS", credentialsMountPath))

	case utils.OSS:
		env = append(env, getEnvVarFromValue("ALICLOUD_APPLICATION_CREDENTIALS", credentialsMountPath))

	case utils.ECS:
		env = append(env, getEnvVarFromSecrets("ECS_ENDPOINT", storeValues.SecretRef.Name, "endpoint"))
		env = append(env, getEnvVarFromSecrets("ECS_ACCESS_KEY_ID", storeValues.SecretRef.Name, "accessKeyID"))
		env = append(env, getEnvVarFromSecrets("ECS_SECRET_ACCESS_KEY", storeValues.SecretRef.Name, "secretAccessKey"))

	case utils.OCS:
		env = append(env, getEnvVarFromValue("OPENSHIFT_APPLICATION_CREDENTIALS", credentialsMountPath))
	}

	return env
}

func getEnvVarFromValue(name, value string) corev1.EnvVar {
	return corev1.EnvVar{
		Name:  name,
		Value: value,
	}
}

func getEnvVarFromField(name, fieldPath string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: fieldPath,
			},
		},
	}
}

func getEnvVarFromSecrets(name, secretName, secretKey string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: secretName,
				},
				Key: secretKey,
			},
		},
	}
}