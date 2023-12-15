/*
Copyright 2023.

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"golang.org/x/sync/errgroup"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	commonUtils "github.com/openshift-kni/lifecycle-agent/utils"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) getSeedImage(
	ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	// Use cluster wide pull-secret by default
	pullSecretFilename := common.ImageRegistryAuthFile

	if ibu.Spec.SeedImageRef.PullSecretRef != nil {
		var pullSecret string
		pullSecret, err := utils.LoadSecretData(
			ctx, r.Client, ibu.Spec.SeedImageRef.PullSecretRef.Name, common.LcaNamespace, corev1.DockerConfigJsonKey,
		)
		if err != nil {
			err = fmt.Errorf("failed to retrieve pull-secret from secret %s, err: %w", ibu.Spec.SeedImageRef.PullSecretRef.Name, err)
			return err
		}

		pullSecretFilename = filepath.Join(utils.IBUWorkspacePath, "seed-pull-secret")
		if err = os.WriteFile(common.PathOutsideChroot(pullSecretFilename), []byte(pullSecret), 0o600); err != nil {
			err = fmt.Errorf("failed to write seed image pull-secret to file %s, err: %w", pullSecretFilename, err)
			return err
		}
		defer os.Remove(common.PathOutsideChroot(pullSecretFilename))
	}

	r.Log.Info("Pulling seed image")
	if _, err := r.Executor.Execute("podman", "pull", "--authfile", pullSecretFilename, ibu.Spec.SeedImageRef.Image); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	return nil

}

func readPrecachingList(imageListFile, clusterRegistry, seedRegistry string) (imageList []string, err error) {
	var content []byte
	content, err = os.ReadFile(common.PathOutsideChroot(imageListFile))
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")

	// Filter out empty lines
	for _, line := range lines {
		if line == "" {
			continue
		}
		image, err := commonUtils.ReplaceImageRegistry(line, clusterRegistry, seedRegistry)
		if err != nil {
			return nil, err
		}
		imageList = append(imageList, image)
	}

	return imageList, nil
}

func (r *ImageBasedUpgradeReconciler) launchPrecaching(ctx context.Context, imageListFile string, ibu *lcav1alpha1.ImageBasedUpgrade) (bool, error) {
	clusterRegistry, err := commonUtils.GetReleaseRegistry(ctx, r.Client)
	if err != nil {
		r.Log.Error(err, "Failed to get cluster registry")
		return false, err
	}

	seedInfo, err := commonUtils.ReadClusterInfoFromFile(
		common.PathOutsideChroot(getSeedManifestPath(common.GetDesiredStaterootName(ibu))))
	if err != nil {
		r.Log.Error(err, "Failed to read seed info")
		return false, err
	}

	imageList, err := readPrecachingList(imageListFile, clusterRegistry, seedInfo.ReleaseRegistry)
	if err != nil {
		err = fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
		return false, err
	}

	// Create pre-cache config using default values
	config := precache.NewConfig(imageList)
	err = r.Precache.CreateJob(ctx, config)
	if err != nil {
		r.Log.Error(err, "Failed to create precaching job")
		return false, err
	}

	return true, nil
}

func (r *ImageBasedUpgradeReconciler) queryPrecachingStatus(ctx context.Context) (status *precache.Status, err error) {
	status, err = r.Precache.QueryJobStatus(ctx)
	if err != nil {
		r.Log.Info("Failed to get precaching job status")
		return
	}

	if status == nil {
		r.Log.Info("Precaching job status is nil")
		return
	}

	if status.Status == precache.Failed {
		return status, precache.ErrFailed
	}

	var logMsg string
	switch {
	case status.Status == precache.Active:
		logMsg = "Precaching in-progress"
	case status.Status == precache.Succeeded:
		logMsg = "Precaching completed"
	}

	// Augment precaching log message data with precache summary report (if available)
	if status.Message != "" {
		logMsg = fmt.Sprintf("%s: %s", logMsg, status.Message)
	}
	r.Log.Info(logMsg)

	return
}

func (r *ImageBasedUpgradeReconciler) SetupStateroot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, imageListFile string) error {
	r.Log.Info("Start setupstateroot")

	defer r.Ops.UnmountAndRemoveImage(ibu.Spec.SeedImageRef.Image)

	workspaceOutsideChroot, err := os.MkdirTemp(common.PathOutsideChroot("/var/tmp"), "")
	if err != nil {
		return fmt.Errorf("failed to create temp directory %w", err)
	}

	defer func() {
		if err := os.RemoveAll(workspaceOutsideChroot); err != nil {
			r.Log.Error(err, "failed to cleanup workspace")
		}
	}()

	workspace, err := filepath.Rel(common.Host, workspaceOutsideChroot)
	if err != nil {
		return fmt.Errorf("failed to get workspace relative path %w", err)
	}
	r.Log.Info("workspace:" + workspace)

	if err = r.Ops.RemountSysroot(); err != nil {
		return fmt.Errorf("failed to remount /sysroot: %w", err)
	}

	mountpoint, err := r.Executor.Execute("podman", "image", "mount", ibu.Spec.SeedImageRef.Image)
	if err != nil {
		return fmt.Errorf("failed to mount seed image: %w", err)
	}

	ostreeRepo := filepath.Join(workspace, "ostree")
	if err = os.Mkdir(common.PathOutsideChroot(ostreeRepo), 0o755); err != nil {
		return fmt.Errorf("failed to create ostree repo directory: %w", err)
	}

	if err := r.Ops.ExtractTarWithSELinux(
		fmt.Sprintf("%s/ostree.tgz", mountpoint), ostreeRepo,
	); err != nil {
		return fmt.Errorf("failed to extract ostree.tgz: %w", err)
	}

	// example:
	// seedBootedID: rhcos-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedDeployment: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedRef: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c
	seedBootedID, err := prep.GetBootedStaterootIDFromRPMOstreeJson(filepath.Join(common.PathOutsideChroot(mountpoint), "rpm-ostree.json"))
	if err != nil {
		return fmt.Errorf("failed to get booted stateroot id: %w", err)
	}
	seedBootedDeployment, err := prep.GetDeploymentFromDeploymentID(seedBootedID)
	if err != nil {
		return err
	}
	seedBootedRef := strings.Split(seedBootedDeployment, ".")[0]

	version, err := prep.GetVersionFromClusterInfoFile(filepath.Join(common.PathOutsideChroot(mountpoint), common.ClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to get version from ClusterInfo: %w", err)
	}

	if version != ibu.Spec.SeedImageRef.Version {
		return fmt.Errorf("version specified in seed image (%s) differs from version in spec (%s)",
			version, ibu.Spec.SeedImageRef.Version)
	}

	osname := common.GetDesiredStaterootName(ibu)

	if err = r.OstreeClient.PullLocal(ostreeRepo); err != nil {
		return fmt.Errorf("failed ostree pull-local: %w", err)
	}

	if err = r.OstreeClient.OSInit(osname); err != nil {
		return fmt.Errorf("failed ostree admin os-init: %w", err)
	}

	kargs, err := prep.BuildKernelArgumentsFromMCOFile(filepath.Join(common.PathOutsideChroot(mountpoint), "mco-currentconfig.json"))
	if err != nil {
		return fmt.Errorf("failed to build kargs: %w", err)
	}

	if err = r.OstreeClient.Deploy(osname, seedBootedRef, kargs); err != nil {
		return fmt.Errorf("failed ostree admin deploy: %w", err)
	}

	deploymentID, err := r.RPMOstreeClient.GetDeploymentID(osname)
	if err != nil {
		return fmt.Errorf("failed to get deploymentID: %w", err)
	}
	deployment, err := prep.GetDeploymentFromDeploymentID(deploymentID)
	if err != nil {
		return err
	}

	if err = common.CopyOutsideChroot(
		filepath.Join(mountpoint, fmt.Sprintf("ostree-%s.origin", seedBootedDeployment)),
		prep.GetDeploymentOriginPath(osname, deployment),
	); err != nil {
		return fmt.Errorf("failed to restore origin file: %w", err)
	}

	if err = r.Ops.ExtractTarWithSELinux(
		filepath.Join(mountpoint, "var.tgz"),
		common.GetStaterootPath(osname),
	); err != nil {
		return fmt.Errorf("failed to restore var directory: %w", err)
	}

	if err := r.Ops.ExtractTarWithSELinux(
		filepath.Join(mountpoint, "etc.tgz"),
		prep.GetDeploymentDirPath(osname, deployment),
	); err != nil {
		return fmt.Errorf("failed to extract seed etc: %w", err)
	}

	if err = prep.RemoveETCDeletions(mountpoint, osname, deployment); err != nil {
		return fmt.Errorf("failed to process etc.deletions: %w", err)
	}

	certsDir := common.PathOutsideChroot(
		filepath.Join(common.GetStaterootPath(osname), "/var/opt/openshift/certs"),
	)
	if err := commonUtils.BackupCertificates(ctx, r.Client, certsDir); err != nil {
		return fmt.Errorf("failed to backup cerificaties: %w", err)
	}

	if err := common.CopyOutsideChroot(filepath.Join(mountpoint, "containers.list"), imageListFile); err != nil {
		return fmt.Errorf("failed to copy image list file: %w", err)
	}

	return nil
}

func (r *ImageBasedUpgradeReconciler) verifyPrecachingCompleteFunc(retries int, interval time.Duration) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		r.Log.Info("Querying pre-caching job for completion...")
		for retry := 0; retry < retries; retry++ {
			status, err := r.queryPrecachingStatus(ctx)
			if err != nil && errors.Is(err, precache.ErrFailed) {
				// precaching job failed - exit immediately
				return false, err
			} else if status != nil {
				if status.Message != "" {
					r.PrepTask.Progress = fmt.Sprintf("Precaching progress: %s", status.Message)
				}
				if status.Status == precache.Succeeded {
					// precaching job succeeded
					return true, nil
				} else if status.Status == precache.Active {
					// precaching job still in-progress
					return false, nil
				}
			}
			// retry after interval
			time.Sleep(interval)
		}
		// failed more than retries times to retrieve precaching status - exit with error
		return false, fmt.Errorf("failed more than %d times to fetch precaching job status", retries)
	}
}

func (r *ImageBasedUpgradeReconciler) prepStageWorker(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (err error) {
	var (
		derivedCtx context.Context
		errGroup   errgroup.Group
	)

	// Create a new context for the worker, derived from the original context
	derivedCtx, r.PrepTask.Cancel = context.WithCancel(ctx)
	defer r.PrepTask.Cancel() // Ensure that the cancel function is called when the prepStageWorker function exits

	errGroup.Go(func() error {
		var ok bool
		imageListFile := filepath.Join(utils.IBUWorkspacePath, "image-list-file")

		// Pull seed image
		select {
		case <-derivedCtx.Done():
			r.Log.Info("Context canceled before pulling seed image")
			return derivedCtx.Err()
		default:
			r.PrepTask.Progress = "Pulling seed image"
			if err = r.getSeedImage(derivedCtx, ibu); err != nil {
				r.Log.Error(err, "failed to pull seed image")
				return err
			}
			r.Log.Info("Successfully pulled seed image")
			r.PrepTask.Progress = "Successfully pulled seed image"
		}

		// Setup state-root
		select {
		case <-derivedCtx.Done():
			r.Log.Info("Context canceled before setting up stateroot")
			return derivedCtx.Err()
		default:
			r.PrepTask.Progress = "Setting up stateroot"
			if err = r.SetupStateroot(derivedCtx, ibu, imageListFile); err != nil {
				r.Log.Error(err, "failed to setup stateroot")
				return err
			}
			r.Log.Info("Successfully setup stateroot")
			r.PrepTask.Progress = "Successfully setup stateroot"
		}

		// Launch precaching job
		select {
		case <-derivedCtx.Done():
			r.Log.Info("Context canceled before creating precaching job")
			return derivedCtx.Err()
		default:
			r.PrepTask.Progress = "Creating precaching job"
			ok, err = r.launchPrecaching(derivedCtx, imageListFile, ibu)
			if err != nil {
				r.Log.Info("Failed to launch pre-caching phase")
				return err
			}
			if !ok {
				return fmt.Errorf("failed to create precaching job")
			}
			r.Log.Info("Successfully created precaching job")
			r.PrepTask.Progress = "Successfully created precaching job"
		}

		// Wait for precaching job to complete
		r.PrepTask.Progress = "Waiting for precaching job to complete"
		interval := 30 * time.Second
		if err = wait.PollUntilContextCancel(derivedCtx, interval, false, r.verifyPrecachingCompleteFunc(5, interval)); err != nil {
			r.Log.Info("Failed to precache images")
			return err
		}

		// Fetch final precaching job report summary
		msg := "Prep completed successfully"
		status, err := r.Precache.QueryJobStatus(ctx)
		if err == nil && status != nil && status.Message != "" {
			msg += fmt.Sprintf(": %s", status.Message)
		}
		r.PrepTask.Progress = msg

		// Prep-stage completed successfully
		return nil
	})

	if err := errGroup.Wait(); err != nil {
		r.Log.Info("Encountered error while running prep-stage worker goroutine", "error", err)
		r.PrepTask.Progress = fmt.Sprintf("Prep failed with error: %v", err)
		return err
	}

	return nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {

	result = doNotRequeue()

	_, err = os.Stat(common.Host)
	if err != nil {
		// fail without /host
		return
	}

	if _, err = os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); os.IsNotExist(err) {
		err = os.Mkdir(common.PathOutsideChroot(utils.IBUWorkspacePath), 0o700)
	}

	if err != nil {
		return
	}

	switch {
	case !r.PrepTask.Active:
		r.PrepTask.done = make(chan struct{})
		r.PrepTask.Active = true
		r.PrepTask.Success = false
		r.PrepTask.Progress = "Prep stage initialized"
		go func() {
			err = r.prepStageWorker(ctx, ibu)
			close(r.PrepTask.done)
			if err != nil {
				r.Log.Error(err, "Prep stage failed with error")
				r.PrepTask.Success = false
			} else {
				r.Log.Info("Prep stage completed successfully!")
				r.PrepTask.Success = true
			}
		}()
		utils.SetPrepStatusInProgress(ibu, r.PrepTask.Progress)
		result = requeueWithShortInterval()
	case r.PrepTask.Active:
		select {
		case <-r.PrepTask.done:
			if r.PrepTask.Success {
				utils.SetPrepStatusCompleted(ibu, r.PrepTask.Progress)
			} else {
				utils.SetPrepStatusFailed(ibu, r.PrepTask.Progress)
			}
			// Reset Task values
			r.PrepTask.Reset()
			result = doNotRequeue()
		default:
			utils.SetPrepStatusInProgress(ibu, r.PrepTask.Progress)
			result = requeueWithShortInterval()
		}
	}

	return
}

func getSeedManifestPath(osname string) string {
	return filepath.Join(
		common.GetStaterootPath(osname),
		filepath.Join(common.SeedDataDir, common.ClusterInfoFileName),
	)
}
