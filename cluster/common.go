// Copyright © 2018 Banzai Cloud
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

package cluster

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/banzaicloud/pipeline/auth"
	"github.com/banzaicloud/pipeline/config"
	"github.com/banzaicloud/pipeline/internal/platform/database"
	"github.com/banzaicloud/pipeline/model"
	pkgCluster "github.com/banzaicloud/pipeline/pkg/cluster"
	pkgCommon "github.com/banzaicloud/pipeline/pkg/common"
	pkgErrors "github.com/banzaicloud/pipeline/pkg/errors"
	modelOracle "github.com/banzaicloud/pipeline/pkg/providers/oracle/model"
	pkgSecret "github.com/banzaicloud/pipeline/pkg/secret"
	"github.com/banzaicloud/pipeline/secret"
	"github.com/banzaicloud/pipeline/utils"
	"github.com/goph/emperror"
	"github.com/pkg/errors"
)

// CommonCluster interface for clusters.
type CommonCluster interface {
	// Entity properties
	GetID() uint
	GetUID() string
	GetOrganizationId() uint
	GetName() string
	GetCloud() string
	GetDistribution() string
	GetLocation() string
	GetCreatedBy() uint

	// Secrets
	GetSecretId() string
	GetSshSecretId() string
	SaveSshSecretId(string) error
	SaveConfigSecretId(string) error
	GetConfigSecretId() string
	GetSecretWithValidation() (*secret.SecretItemResponse, error)

	// Persistence
	Persist(string, string) error
	UpdateStatus(string, string) error
	DeleteFromDatabase() error

	// Cluster management
	CreateCluster() error
	ValidateCreationFields(r *pkgCluster.CreateClusterRequest) error
	UpdateCluster(*pkgCluster.UpdateClusterRequest, uint) error
	UpdateNodePools(*pkgCluster.UpdateNodePoolsRequest, uint) error
	CheckEqualityToUpdate(*pkgCluster.UpdateClusterRequest) error
	AddDefaultsToUpdate(*pkgCluster.UpdateClusterRequest)
	DeleteCluster() error

	// Kubernetes
	DownloadK8sConfig() ([]byte, error)
	GetAPIEndpoint() (string, error)
	GetK8sIpv4Cidrs() (*pkgCluster.Ipv4Cidrs, error)
	GetK8sConfig() ([]byte, error)
	RequiresSshPublicKey() bool
	RbacEnabled() bool
	NeedAdminRights() bool
	GetKubernetesUserName() (string, error)

	// Cluster info
	GetStatus() (*pkgCluster.GetClusterStatusResponse, error)
	IsReady() (bool, error)
	ListNodeNames() (pkgCommon.NodeNames, error)
	NodePoolExists(nodePoolName string) bool

	// Set Get flags
	GetSecurityScan() bool
	SetSecurityScan(scan bool)
	GetLogging() bool
	SetLogging(l bool)
	GetMonitoring() bool
	SetMonitoring(m bool)
	GetServiceMesh() bool
	SetServiceMesh(m bool)
}

// CommonClusterBase holds the fields that is common to all cluster types
// also provides default implementation for common interface methods.
type CommonClusterBase struct {
	secret    *secret.SecretItemResponse
	sshSecret *secret.SecretItemResponse

	config []byte
}

// ErrConfigNotExists means that a cluster has no kubeconfig stored in vault (probably didn't successfully start yet)
var ErrConfigNotExists = fmt.Errorf("Kubernetes config is not available for the cluster")

// RequiresSshPublicKey returns true if an ssh public key is needed for the cluster for bootstrapping it.
// The default is false.
func (c *CommonClusterBase) RequiresSshPublicKey() bool {
	return false
}

func (c *CommonClusterBase) getSecret(cluster CommonCluster) (*secret.SecretItemResponse, error) {
	if c.secret == nil {
		s, err := getSecret(cluster.GetOrganizationId(), cluster.GetSecretId())
		if err != nil {
			return nil, err
		}
		c.secret = s
	}

	err := c.secret.ValidateSecretType(cluster.GetCloud())
	if err != nil {
		return nil, err
	}

	return c.secret, err
}

func (c *CommonClusterBase) getSshSecret(cluster CommonCluster) (*secret.SecretItemResponse, error) {
	if c.sshSecret == nil {
		s, err := getSecret(cluster.GetOrganizationId(), cluster.GetSshSecretId())
		if err != nil {
			return nil, emperror.With(err, "cluster", cluster.GetName())
		}
		c.sshSecret = s

		err = c.sshSecret.ValidateSecretType(pkgSecret.SSHSecretType)
		if err != nil {
			return nil, emperror.With(err, "cluster", cluster.GetName())
		}
	}

	return c.sshSecret, nil
}

func (c *CommonClusterBase) getConfig(cluster CommonCluster) ([]byte, error) {
	if c.config == nil {
		var loadedConfig []byte
		secretId := cluster.GetConfigSecretId()
		if secretId == "" {
			return nil, ErrConfigNotExists
		}
		configSecret, err := getSecret(cluster.GetOrganizationId(), secretId)
		if err != nil {
			return nil, errors.Wrap(err, "can't get config from Vault")
		}
		configStr, err := base64.StdEncoding.DecodeString(configSecret.GetValue(pkgSecret.K8SConfig))
		if err != nil {
			return nil, errors.Wrap(err, "can't decode Kubernetes config")
		}
		loadedConfig = []byte(configStr)

		c.config = loadedConfig
	}
	return c.config, nil
}

// StoreKubernetesConfig stores the given K8S config in vault
func StoreKubernetesConfig(cluster CommonCluster, config []byte) error {

	encodedConfig := utils.EncodeStringToBase64(string(config))

	organizationID := cluster.GetOrganizationId()
	clusterUidTag := fmt.Sprintf("clusterUID:%s", cluster.GetUID())

	createSecretRequest := secret.CreateSecretRequest{
		Name: fmt.Sprintf("cluster-%d-config", cluster.GetID()),
		Type: pkgSecret.K8SConfig,
		Values: map[string]string{
			pkgSecret.K8SConfig: encodedConfig,
		},
		Tags: []string{
			pkgSecret.TagKubeConfig,
			pkgSecret.TagBanzaiReadonly,
			clusterUidTag,
		},
	}

	secretID := secret.GenerateSecretID(&createSecretRequest)

	// Try to get the secret version first
	if configSecret, err := getSecret(organizationID, secretID); err != nil && err != secret.ErrSecretNotExists {
		return err
	} else if configSecret != nil {
		createSecretRequest.Version = &(configSecret.Version)
	}

	err := secret.Store.Update(organizationID, secretID, &createSecretRequest)
	if err != nil {
		return err
	}

	log.Info("Kubeconfig stored in vault")

	log.Info("Update cluster model in DB with config secret id")
	if err := cluster.SaveConfigSecretId(secretID); err != nil {
		log.Errorf("Error during saving config secret id: %s", err.Error())
		return err
	}

	return nil
}

func getSecret(organizationId uint, secretId string) (*secret.SecretItemResponse, error) {
	return secret.Store.Get(organizationId, secretId)
}

// GetCommonClusterFromModel extracts CommonCluster from a ClusterModel
func GetCommonClusterFromModel(modelCluster *model.ClusterModel) (CommonCluster, error) {

	db := config.DB()

	cloudType := modelCluster.Cloud
	distribution := modelCluster.Distribution

	if distribution == "banzaicloud" {
		return createCommonClusterWithDistributionFromModel(modelCluster)
	}

	switch cloudType {
	case pkgCluster.Alibaba:
		//Create Alibaba struct
		alibabaCluster, err := CreateACSKClusterFromModel(modelCluster)
		if err != nil {
			return nil, err
		}

		err = db.Where(model.ACSKClusterModel{ID: alibabaCluster.modelCluster.ID}).First(&alibabaCluster.modelCluster.ACSK).Error
		if err != nil {
			return nil, err
		}

		err = db.Model(&alibabaCluster.modelCluster.ACSK).Related(&alibabaCluster.modelCluster.ACSK.NodePools, "NodePools").Error
		if err != nil {
			return nil, err
		}

		return alibabaCluster, nil

	case pkgCluster.Amazon:
		//Create Amazon EKS struct
		eksCluster, err := CreateEKSClusterFromModel(modelCluster)
		if err != nil {
			return nil, err
		}

		err = db.Where(model.EKSClusterModel{ClusterID: eksCluster.modelCluster.ID}).First(&eksCluster.modelCluster.EKS).Error
		if err != nil {
			return nil, err
		}
		err = db.Model(&eksCluster.modelCluster.EKS).Related(&eksCluster.modelCluster.EKS.NodePools, "NodePools").Error
		if err != nil {
			return nil, err
		}

		err = db.Model(&eksCluster.modelCluster.EKS).Related(&eksCluster.modelCluster.EKS.Subnets, "Subnets").Error

		return eksCluster, err

	case pkgCluster.Azure:
		// Create Azure struct
		aksCluster, err := CreateAKSClusterFromModel(modelCluster)
		if err != nil {
			return nil, err
		}

		err = db.Where(model.AKSClusterModel{ID: aksCluster.modelCluster.ID}).First(&aksCluster.modelCluster.AKS).Error
		if err != nil {
			return nil, err
		}
		err = db.Model(&aksCluster.modelCluster.AKS).Related(&aksCluster.modelCluster.AKS.NodePools, "NodePools").Error

		return aksCluster, err

	case pkgCluster.Google:
		// Create Google struct
		gkeCluster, err := CreateGKEClusterFromModel(modelCluster)
		if err != nil {
			return nil, err
		}

		return gkeCluster, err

	case pkgCluster.Dummy:
		dummyCluster, err := CreateDummyClusterFromModel(modelCluster)
		if err != nil {
			return nil, err
		}

		err = db.Where(model.DummyClusterModel{ID: dummyCluster.modelCluster.ID}).First(&dummyCluster.modelCluster.Dummy).Error

		return dummyCluster, err

	case pkgCluster.Kubernetes:
		// Create Kubernetes struct
		kubernetesCluster, err := CreateKubernetesClusterFromModel(modelCluster)
		if err != nil {
			return nil, err
		}

		err = db.Where(model.KubernetesClusterModel{ID: kubernetesCluster.modelCluster.ID}).First(&kubernetesCluster.modelCluster.Kubernetes).Error
		if database.IsRecordNotFoundError(err) {
			// metadata not set so there's no properties in DB
			log.Warnf(err.Error())
			err = nil
		}

		return kubernetesCluster, err

	case pkgCluster.Oracle:
		// Create Oracle struct
		okeCluster, err := CreateOKEClusterFromModel(modelCluster)
		if err != nil {
			return nil, err
		}

		err = db.Where(modelOracle.Cluster{ClusterModelID: okeCluster.modelCluster.ID}).Preload("NodePools.Subnets").Preload("NodePools.Labels").First(&okeCluster.modelCluster.OKE).Error

		return okeCluster, err
	}

	return nil, pkgErrors.ErrorNotSupportedCloudType
}

//CreateCommonClusterFromRequest creates a CommonCluster from a request
func CreateCommonClusterFromRequest(createClusterRequest *pkgCluster.CreateClusterRequest, orgId, userId uint) (CommonCluster, error) {

	if err := createClusterRequest.AddDefaults(); err != nil {
		return nil, err
	}

	// validate request
	if err := createClusterRequest.Validate(); err != nil {
		return nil, err
	}

	if createClusterRequest.Distribution != "" {
		return createCommonClusterWithDistributionFromRequest(createClusterRequest, orgId, userId)
	}

	cloudType := createClusterRequest.Cloud
	switch cloudType {
	case pkgCluster.Alibaba:
		//Create Alibaba struct
		alibabaCluster, err := CreateACSKClusterFromRequest(createClusterRequest, orgId, userId)
		if err != nil {
			return nil, err
		}
		return alibabaCluster, nil

	case pkgCluster.Amazon:
		//Create EKS struct
		eksCluster, err := CreateEKSClusterFromRequest(createClusterRequest, orgId, userId)
		if err != nil {
			return nil, err
		}
		return eksCluster, nil

	case pkgCluster.Azure:
		// Create AKS struct
		aksCluster, err := CreateAKSClusterFromRequest(createClusterRequest, orgId, userId)
		if err != nil {
			return nil, err
		}
		return aksCluster, nil

	case pkgCluster.Google:
		// Create GKE struct
		gkeCluster, err := CreateGKEClusterFromRequest(createClusterRequest, orgId, userId)
		if err != nil {
			return nil, err
		}
		return gkeCluster, nil

	case pkgCluster.Dummy:
		// Create Dummy struct
		dummy, err := CreateDummyClusterFromRequest(createClusterRequest, orgId, userId)
		if err != nil {
			return nil, err
		}

		return dummy, nil

	case pkgCluster.Kubernetes:
		// Create Kubernetes struct
		kubeCluster, err := CreateKubernetesClusterFromRequest(createClusterRequest, orgId, userId)
		if err != nil {
			return nil, err
		}
		return kubeCluster, nil

	case pkgCluster.Oracle:
		// Create OKE struct
		okeCluster, err := CreateOKEClusterFromRequest(createClusterRequest, orgId, userId)
		if err != nil {
			return nil, err
		}
		return okeCluster, nil

	}

	return nil, pkgErrors.ErrorNotSupportedCloudType
}

//createCommonClusterWithDistributionFromRequest creates a CommonCluster from a request
func createCommonClusterWithDistributionFromRequest(createClusterRequest *pkgCluster.CreateClusterRequest, orgId, userId uint) (*EC2ClusterBanzaiCloudDistribution, error) {
	// Whitelist supported distribution.
	if createClusterRequest.Distribution != pkgCluster.BanzaiCloud {
		return nil, pkgErrors.ErrorNotSupportedDistributionType
	}

	switch createClusterRequest.Cloud {
	case pkgCluster.Amazon:
		return CreateEC2ClusterBanzaiCloudDistributionFromRequest(createClusterRequest, orgId, userId)

	default:
		return nil, pkgErrors.ErrorNotSupportedCloudType
	}
}

func createCommonClusterWithDistributionFromModel(modelCluster *model.ClusterModel) (*EC2ClusterBanzaiCloudDistribution, error) {
	if modelCluster.Distribution != pkgCluster.BanzaiCloud {
		return nil, pkgErrors.ErrorNotSupportedDistributionType
	}

	switch modelCluster.Cloud {
	case pkgCluster.Amazon:
		return CreateEC2ClusterBanzaiCloudDistributionFromModel(modelCluster)

	default:
		return nil, pkgErrors.ErrorNotSupportedCloudType
	}
}

// CleanStateStore deletes state store folder by cluster name
func CleanStateStore(path string) error {
	if len(path) != 0 {
		stateStorePath := config.GetStateStorePath(path)
		return os.RemoveAll(stateStorePath)
	}
	return pkgErrors.ErrStateStorePathEmpty
}

// CleanHelmFolder deletes helm path
func CleanHelmFolder(organizationName string) error {
	helmPath := config.GetHelmPath(organizationName)
	return os.RemoveAll(helmPath)
}

// GetUserIdAndName returns userId and userName from DB
func GetUserIdAndName(modelCluster *model.ClusterModel) (userId uint, userName string) {
	userId = modelCluster.CreatedBy
	userName = auth.GetUserNickNameById(userId)
	return
}

// NewCreatorBaseFields creates a new CreatorBaseFields instance from createdAt and createdBy
func NewCreatorBaseFields(createdAt time.Time, createdBy uint) *pkgCommon.CreatorBaseFields {

	var userName string
	if createdBy != 0 {
		userName = auth.GetUserNickNameById(createdBy)
	}

	return &pkgCommon.CreatorBaseFields{
		CreatedAt:   createdAt,
		CreatorName: userName,
		CreatorId:   createdBy,
	}
}
