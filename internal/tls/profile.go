/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tls

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	openshifttls "github.com/openshift/controller-runtime-common/pkg/tls"
	"github.com/openshift/library-go/pkg/crypto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Profile holds the cluster TLS security profile and the controller-runtime
// TLSOpts that should be applied to operator TLS servers.
type Profile struct {
	TLSOpts     []func(*tls.Config)
	profileSpec *configv1.TLSProfileSpec
	adherence   configv1.TLSAdherencePolicy
}

// GetProfileInfo reads apiservers.config.openshift.io/cluster and builds TLSOpts
// for the operator's TLS servers (metrics, webhook).
//
// When ShouldHonorClusterTLSProfile is true, TLSOpts apply the cluster
// tlsSecurityProfile (min version, ciphers, and curve/group preferences).
// Otherwise TLSOpts is empty and the servers keep their existing TLS defaults.
func GetProfileInfo(ctx context.Context, client ctrlclient.Client) (Profile, error) {
	log := logr.FromContextOrDiscard(ctx)

	apiServer := &configv1.APIServer{}
	if err := client.Get(ctx, types.NamespacedName{Name: openshifttls.APIServerName}, apiServer); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			log.Info("OpenShift TLS profile API not available")
			return Profile{}, nil
		}

		return Profile{}, fmt.Errorf("failed to fetch apiserver.config.openshift.io/%s: %w", openshifttls.APIServerName, err)
	}

	profileSpec, err := openshifttls.GetTLSProfileSpec(apiServer.Spec.TLSSecurityProfile)
	if err != nil {
		return Profile{}, fmt.Errorf("failed to get TLS profile spec: %w", err)
	}

	profile := Profile{
		profileSpec: &profileSpec,
		adherence:   apiServer.Spec.TLSAdherence,
	}

	if !crypto.ShouldHonorClusterTLSProfile(apiServer.Spec.TLSAdherence) {
		log.Info("Not honoring cluster TLS profile due to adherence policy", "adherence", apiServer.Spec.TLSAdherence)
		return profile, nil
	}

	log.Info("Honoring cluster TLS profile", "adherence", apiServer.Spec.TLSAdherence)

	tlsConfigFunc, unsupportedCiphers := openshifttls.NewTLSConfigFromProfile(profileSpec)
	for _, cipher := range unsupportedCiphers {
		log.Info("Cipher suite not available in this Go version, skipping", "cipher", cipher)
	}

	profile.TLSOpts = []func(*tls.Config){tlsConfigFunc}
	return profile, nil
}

// SetupProfileWatch registers a watcher for tlsSecurityProfile and tlsAdherence
// changes. onChange is invoked when either field changes so the caller can
// cancel the manager context and let the Deployment restart the pod.
func (p *Profile) SetupProfileWatch(ctx context.Context, mgr ctrl.Manager, onChange func()) error {
	log := logr.FromContextOrDiscard(ctx)

	if p.profileSpec == nil {
		return nil
	}

	tlsProfileWatcher := &openshifttls.SecurityProfileWatcher{
		Client:                    mgr.GetClient(),
		InitialTLSProfileSpec:     *p.profileSpec,
		InitialTLSAdherencePolicy: p.adherence,
		OnProfileChange: func(_ context.Context, oldProfile, newProfile configv1.TLSProfileSpec) {
			if !crypto.ShouldHonorClusterTLSProfile(p.adherence) {
				log.Info("TLS security profile changed but not honored due to adherence policy", "adherence", p.adherence)
				return
			}
			log.Info("TLS security profile changed",
				"oldMinVersion", oldProfile.MinTLSVersion, "oldCiphers", len(oldProfile.Ciphers), "oldGroups", len(oldProfile.Groups),
				"newMinVersion", newProfile.MinTLSVersion, "newCiphers", len(newProfile.Ciphers), "newGroups", len(newProfile.Groups))
			onChange()
		},
		OnAdherencePolicyChange: func(_ context.Context, oldTLSAdherencePolicy, newTLSAdherencePolicy configv1.TLSAdherencePolicy) {
			log.Info("TLS adherence policy changed", "old", oldTLSAdherencePolicy, "new", newTLSAdherencePolicy)
			onChange()
		},
	}

	if err := tlsProfileWatcher.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to setup TLS profile watcher: %w", err)
	}

	return nil
}
