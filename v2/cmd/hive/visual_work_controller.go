package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
)

var normalVisualWorkService *visualcontroller.Controller

type dashboardVisualWorkAudit struct{ server *dashboard.Server }

func (sink dashboardVisualWorkAudit) RecordVisualWorkAudit(_ context.Context, event visualcontroller.AuditEvent) error {
	if sink.server == nil {
		return errors.New("normal dashboard audit sink is unavailable")
	}
	detail := fmt.Sprintf("stage=%s source=%s finding=%s detail=%s", event.Stage, event.SourceExternalRef, event.RepositoryFingerprint, event.Detail)
	if event.Decision != nil {
		detail += fmt.Sprintf(" decision=%s request=%s", event.Decision.Code, event.Decision.RequestSHA256)
	}
	sink.server.AuditLog("governor", "visual_work", detail, "")
	return nil
}

func loadCurrentVisualWorkContract(normal *config.Config) (integrated.Config, bool, error) {
	if normal == nil {
		return integrated.Config{}, false, errors.New("normal Hive config is required")
	}
	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || !exists {
		return integrated.Config{}, exists, err
	}
	if !normalProjectContainsRepository(normal.Project, installed.Repository) {
		return integrated.Config{}, false, fmt.Errorf("installed Visual Hive repository %s is outside normal Hive project scope", installed.Repository)
	}
	return installed, true, nil
}

func loadAuthoritativeVisualWorkContract() (integrated.Config, bool, error) {
	stateDir, exists, err := integrated.CurrentState(integratedStateRoot())
	if err != nil || !exists {
		return integrated.Config{}, exists, err
	}
	store, err := integrated.NewStore(stateDir)
	if err != nil {
		return integrated.Config{}, false, err
	}
	installed, err := store.Load()
	if err != nil {
		return integrated.Config{}, false, err
	}
	return installed, true, nil
}

func normalProjectContainsRepository(project config.ProjectConfig, repository string) bool {
	repository = strings.ToLower(strings.TrimSpace(repository))
	for _, configured := range project.Repos {
		configured = strings.ToLower(strings.TrimSpace(configured))
		if !strings.Contains(configured, "/") && strings.TrimSpace(project.Org) != "" {
			configured = strings.ToLower(strings.TrimSpace(project.Org)) + "/" + configured
		}
		if configured == repository {
			return true
		}
	}
	return false
}

func importNormalVisualWork(ctx context.Context, source hivegithub.VerifiedVisualHiveArtifact) (visualcontroller.Result, error) {
	if normalVisualWorkService == nil {
		return visualcontroller.Result{}, errors.New("normal Visual Hive work service is not configured")
	}
	return normalVisualWorkService.Import(ctx, source)
}

func visualLifecycleForInstalledContract(installed integrated.Config) (*visualhive.LifecycleStore, error) {
	return visualhive.NewLifecycleStore(filepath.Join(installed.StateDir, "visual-hive"))
}
