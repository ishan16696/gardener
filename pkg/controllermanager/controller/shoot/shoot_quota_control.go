// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package shoot

import (
	"context"
	"fmt"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardencoreinformers "github.com/gardener/gardener/pkg/client/core/informers/externalversions/core/v1beta1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap"
	"github.com/gardener/gardener/pkg/client/kubernetes/clientmap/keys"
	"github.com/gardener/gardener/pkg/controllermanager/apis/config"
	"github.com/gardener/gardener/pkg/operation/common"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (c *Controller) shootQuotaAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	c.shootQuotaQueue.Add(key)
}

func (c *Controller) shootQuotaDelete(obj interface{}) {
	shoot, ok := obj.(*gardencorev1beta1.Shoot)
	if shoot == nil || !ok {
		return
	}
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	c.shootQuotaQueue.Done(key)
}

// NewShootQuotaReconciler creates a new instance of a reconciler which checks handles Shoots using SecretBindings that
// references Quotas.
func NewShootQuotaReconciler(l logrus.FieldLogger, cfg config.ShootQuotaControllerConfiguration, clientMap clientmap.ClientMap, k8sGardenCoreInformers gardencoreinformers.Interface) reconcile.Reconciler {
	return &shootQuotaReconciler{
		logger:                 l,
		cfg:                    cfg,
		clientMap:              clientMap,
		k8sGardenCoreInformers: k8sGardenCoreInformers,
	}
}

type shootQuotaReconciler struct {
	logger                 logrus.FieldLogger
	cfg                    config.ShootQuotaControllerConfiguration
	clientMap              clientmap.ClientMap
	k8sGardenCoreInformers gardencoreinformers.Interface
}

func (r *shootQuotaReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	gardenClient, err := r.clientMap.GetClient(ctx, keys.ForGarden())
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get garden client: %w", err)
	}

	shoot := &gardencorev1beta1.Shoot{}
	if err := gardenClient.Client().Get(ctx, request.NamespacedName, shoot); err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.Infof("Object %q is gone, stop reconciling: %v", request.Name, err)
			return reconcile.Result{}, nil
		}
		r.logger.Infof("Unable to retrieve object %q from store: %v", request.Name, err)
		return reconcile.Result{}, err
	}

	secretBinding, err := r.k8sGardenCoreInformers.SecretBindings().Lister().SecretBindings(shoot.Namespace).Get(shoot.Spec.SecretBindingName)
	if err != nil {
		return reconcile.Result{}, err
	}

	var clusterLifeTime *int32

	for _, quotaRef := range secretBinding.Quotas {
		quota, err := r.k8sGardenCoreInformers.Quotas().Lister().Quotas(quotaRef.Namespace).Get(quotaRef.Name)
		if err != nil {
			return reconcile.Result{}, err
		}

		if quota.Spec.ClusterLifetimeDays == nil {
			continue
		}
		if clusterLifeTime == nil || *quota.Spec.ClusterLifetimeDays < *clusterLifeTime {
			clusterLifeTime = quota.Spec.ClusterLifetimeDays
		}
	}

	// If the Shoot has no Quotas referenced (anymore) or if the referenced Quotas does not have a clusterLifetime,
	// then we will not check for cluster lifetime expiration, even if the Shoot has a clusterLifetime timestamp already annotated.
	if clusterLifeTime == nil {
		return reconcile.Result{RequeueAfter: r.cfg.SyncPeriod.Duration}, nil
	}

	expirationTime, exits := shoot.Annotations[common.ShootExpirationTimestamp]
	if !exits {
		expirationTime = shoot.CreationTimestamp.Add(time.Duration(*clusterLifeTime*24) * time.Hour).Format(time.RFC3339)
		metav1.SetMetaDataAnnotation(&shoot.ObjectMeta, common.ShootExpirationTimestamp, expirationTime)

		shootUpdated, err := gardenClient.GardenCore().CoreV1beta1().Shoots(shoot.Namespace).Update(ctx, shoot, kubernetes.DefaultUpdateOptions())
		if err != nil {
			return reconcile.Result{}, err
		}
		shoot = shootUpdated
	}

	expirationTimeParsed, err := time.Parse(time.RFC3339, expirationTime)
	if err != nil {
		return reconcile.Result{}, err
	}

	if time.Now().UTC().After(expirationTimeParsed.UTC()) {
		r.logger.Info("[SHOOT QUOTA] Shoot cluster lifetime expired. Shoot will be deleted.")

		// We have to annotate the Shoot to confirm the deletion.
		if err := common.ConfirmDeletion(ctx, gardenClient.Client(), shoot); err != nil {
			return reconcile.Result{}, err
		}

		// Now we are allowed to delete the Shoot (to set the deletionTimestamp).
		if err := gardenClient.GardenCore().CoreV1beta1().Shoots(shoot.Namespace).Delete(ctx, shoot.Name, metav1.DeleteOptions{}); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{RequeueAfter: r.cfg.SyncPeriod.Duration}, nil
}
