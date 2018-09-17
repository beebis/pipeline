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

package azure

import (
	"context"
	"errors"
	"time"

	"github.com/banzaicloud/pipeline/internal/platform/zaplog"
	"go.uber.org/cadence/activity"
	"go.uber.org/zap"
)

const CreateStorageAccountActivityType = "azure-create-storage-account"

type CreateStorageAccountActivityContext struct {
	OrganizationID uint
	SecretID       string
	Location       string
	ResourceGroup  string
	StorageAccount string
}

type CreateStorageAccountActivity struct {
	clientFactory *StorageAccountClientFactory
}

func NewCreateStorageAccountActivity(clientFactory *StorageAccountClientFactory) *CreateStorageAccountActivity {
	return &CreateStorageAccountActivity{
		clientFactory: clientFactory,
	}
}

func (a *CreateStorageAccountActivity) Name() string {
	return CreateStorageAccountActivityType
}

func (a *CreateStorageAccountActivity) Execute(ctx context.Context, activityContext CreateStorageAccountActivityContext) error {
	logger := activity.GetLogger(ctx).With( // TODO: add correlation ID from API request (if any)
		zap.Uint("organization-id", activityContext.OrganizationID),
		zap.String("secret-id", activityContext.SecretID),
		zap.String("location", activityContext.Location),
		zap.String("resource-group", activityContext.ResourceGroup),
		zap.String("storage-account", activityContext.StorageAccount),
	)

	logger.Info("creating storage account")

	time.Sleep(1 * time.Minute)

	client, err := a.clientFactory.New(activityContext.OrganizationID, activityContext.SecretID)
	if err != nil {
		zaplog.LogError(logger, err) // TODO: use error handler

		return errors.New("failed to initialize resource group client")
	}

	err = CreateStorageAccount(ctx, client, activityContext.ResourceGroup, activityContext.StorageAccount, activityContext.Location)
	if err != nil {
		zaplog.LogError(logger, err) // TODO: use error handler

		return errors.New("failed to create storage account") // TODO: return custom errors for business errors
	}

	logger.Info("storage account successfully created")

	return nil
}
