package controllers

import (
	"errors"
	"fmt"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/go-logr/logr"
	oadpv1alpha1 "github.com/openshift/oadp-operator/api/v1alpha1"
	"github.com/openshift/oadp-operator/pkg/credentials"
)

// ValidateDataProtectionCR function validates the DPA CR, returns true if valid, false otherwise
// it calls other validation functions to validate the DPA CR
// TODO: #1129 Clean up duplicate logic for validating backupstoragelocations and volumesnapshotlocations in dpa
func (r *DPAReconciler) ValidateDataProtectionCR(log logr.Logger) (bool, error) {
	dpa := oadpv1alpha1.DataProtectionApplication{}
	if err := r.Get(r.Context, r.NamespacedName, &dpa); err != nil {
		return false, err
	}
	if dpa.Spec.Configuration == nil || dpa.Spec.Configuration.Velero == nil {
		return false, errors.New("DPA CR Velero configuration cannot be nil")
	}

	if dpa.Spec.Configuration.Restic != nil && dpa.Spec.Configuration.NodeAgent != nil {
		return false, errors.New("DPA CR cannot have restic (deprecated in OADP 1.3) as well as nodeAgent options at the same time")
	}

	if dpa.Spec.Configuration.Velero.NoDefaultBackupLocation {
		if len(dpa.Spec.BackupLocations) != 0 {
			return false, errors.New("DPA CR Velero configuration cannot have backup locations if noDefaultBackupLocation is set")
		}
		if dpa.BackupImages() {
			return false, errors.New("backupImages needs to be set to false when noDefaultBackupLocation is set")
		}
	} else {
		if len(dpa.Spec.BackupLocations) == 0 {
			return false, errors.New("no backupstoragelocations configured, ensure a backupstoragelocation has been configured or use the noDefaultBackupLocation flag")
		}
	}

	if validBsl, err := r.ValidateBackupStorageLocations(dpa); !validBsl || err != nil {
		return validBsl, err
	}
	if validVsl, err := r.ValidateVolumeSnapshotLocations(dpa); !validVsl || err != nil {
		return validVsl, err
	}

	// check for VSM/Volsync DataMover (OADP 1.2 or below) syntax
	for _, plugin := range dpa.Spec.Configuration.Velero.DefaultPlugins {
		if plugin == oadpv1alpha1.DefaultPluginVSM {
			return false, errors.New("Delete vsm from spec.configuration.velero.defaultPlugins and dataMover object from spec.features, they are not used in OADP 1.3")
		}
	}
	if dpa.Spec.Features != nil && dpa.Spec.Features.DataMover != nil {
		return false, errors.New("Delete vsm from spec.configuration.velero.defaultPlugins and dataMover object from spec.features, they are not used in OADP 1.3")
	}

	if val, found := dpa.Spec.UnsupportedOverrides[oadpv1alpha1.OperatorTypeKey]; found && val != oadpv1alpha1.OperatorTypeMTC {
		return false, errors.New("only mtc operator type override is supported")
	}

	if _, err := r.ValidateVeleroPlugins(r.Log); err != nil {
		return false, err
	}

	// TODO refactor to call functions only once
	// they are called here to check error, and then after to get value
	if _, err := r.getVeleroResourceReqs(&dpa); err != nil {
		return false, err
	}
	if _, err := getResticResourceReqs(&dpa); err != nil {
		return false, err
	}
	if _, err := getNodeAgentResourceReqs(&dpa); err != nil {
		return false, err
	}

	return true, nil
}

// empty struct to use as map value
type empty struct{}

// For later: Move this code into validator.go when more need for validation arises
// TODO: if multiple default plugins exist, ensure we validate all of them.
// Right now its sequential validation
func (r *DPAReconciler) ValidateVeleroPlugins(log logr.Logger) (bool, error) {
	dpa := oadpv1alpha1.DataProtectionApplication{}
	if err := r.Get(r.Context, r.NamespacedName, &dpa); err != nil {
		return false, err
	}

	providerNeedsDefaultCreds, hasCloudStorage, err := r.noDefaultCredentials(dpa)
	if err != nil {
		return false, err
	}

	snapshotLocationsProviders := make(map[string]bool)
	for _, location := range dpa.Spec.SnapshotLocations {
		if location.Velero != nil {
			provider := strings.TrimPrefix(location.Velero.Provider, veleroIOPrefix)
			snapshotLocationsProviders[provider] = true
		}
	}

	foundAWSPlugin := false
	foundLegacyAWSPlugin := false
	for _, plugin := range dpa.Spec.Configuration.Velero.DefaultPlugins {
		pluginSpecificMap, ok := credentials.PluginSpecificFields[plugin]
		pluginNeedsCheck, foundInBSLorVSL := providerNeedsDefaultCreds[pluginSpecificMap.ProviderName]

		// "aws" and "legacy-aws" cannot both be specified
		if plugin == oadpv1alpha1.DefaultPluginAWS {
			foundAWSPlugin = true
		}
		if plugin == oadpv1alpha1.DefaultPluginLegacyAWS {
			foundLegacyAWSPlugin = true
		}

		if foundInVSL := snapshotLocationsProviders[string(plugin)]; foundInVSL {
			pluginNeedsCheck = true
		}
		if !foundInBSLorVSL && !hasCloudStorage {
			pluginNeedsCheck = true
		}
		if ok && pluginSpecificMap.IsCloudProvider && pluginNeedsCheck && !dpa.Spec.Configuration.Velero.NoDefaultBackupLocation && !dpa.Spec.Configuration.Velero.HasFeatureFlag("no-secret") {
			secretNamesToValidate := mapset.NewSet[string]()
			// check specified credentials in backup locations exists in the cluster
			for _, location := range dpa.Spec.BackupLocations {
				if location.Velero != nil {
					provider := strings.TrimPrefix(location.Velero.Provider, veleroIOPrefix)
					if provider == string(plugin) && location.Velero != nil {
						if location.Velero.Credential != nil {
							secretNamesToValidate.Add(location.Velero.Credential.Name)
						} else {
							secretNamesToValidate.Add(pluginSpecificMap.SecretName)
						}
					}
				}
			}
			// check specified credentials in snapshot locations exists in the cluster
			for _, location := range dpa.Spec.SnapshotLocations {
				if location.Velero != nil {
					provider := strings.TrimPrefix(location.Velero.Provider, veleroIOPrefix)
					if provider == string(plugin) && location.Velero != nil {
						if location.Velero.Credential != nil {
							secretNamesToValidate.Add(location.Velero.Credential.Name)
						} else {
							secretNamesToValidate.Add(pluginSpecificMap.SecretName)
						}
					}
				}
			}
			for _, secretName := range secretNamesToValidate.ToSlice() {
				_, err := r.getProviderSecret(secretName)
				if err != nil {
					r.Log.Info(fmt.Sprintf("error validating %s provider secret:  %s/%s", string(plugin), r.NamespacedName.Namespace, secretName))
					return false, err
				}
			}
		}
	}

	if foundAWSPlugin && foundLegacyAWSPlugin {
		return false, fmt.Errorf("%s and %s can not be both specified in DPA spec.configuration.velero.defaultPlugins", oadpv1alpha1.DefaultPluginAWS, oadpv1alpha1.DefaultPluginLegacyAWS)
	}

	return true, nil
}
