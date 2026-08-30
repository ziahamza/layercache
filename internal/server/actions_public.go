package server

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/remote"
)

type actionsPublicResolver struct {
	client *remote.PublicClient
}

func (resolver actionsPublicResolver) Resolve(
	ctx context.Context,
	request actionscache.PublicResolveRequest,
) (actionscache.PublicResolution, error) {
	download, err := resolver.client.Get(ctx, request.Expected)
	if err != nil {
		return actionscache.PublicResolution{}, classifyActionsPublicError(ctx, err)
	}
	return actionscache.PublicResolution{Envelope: download.Envelope, Archive: download.Body}, nil
}

func (resolver actionsPublicResolver) Revalidate(
	ctx context.Context,
	request actionscache.PublicResolveRequest,
) (publictrust.Envelope, error) {
	_, envelope, _, err := resolver.client.Resolve(ctx, request.Expected)
	if err != nil {
		return publictrust.Envelope{}, classifyActionsPublicError(ctx, err)
	}
	return envelope, nil
}

func classifyActionsPublicError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, remote.ErrMiss) {
		return publictrust.ErrNotFound
	}
	for _, authoritative := range []error{
		publictrust.ErrNotFound,
		publictrust.ErrRevoked,
		publictrust.ErrAmbiguous,
		publictrust.ErrSignature,
		publictrust.ErrIdentity,
		publictrust.ErrExpired,
	} {
		if errors.Is(err, authoritative) {
			return err
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) || isUnavailablePublicHTTPStatus(err) {
		return errors.Join(actionscache.ErrPublicOffline, err)
	}
	return err
}

func isUnavailablePublicHTTPStatus(err error) bool {
	message := err.Error()
	for _, prefix := range []string{
		"Public Cache resolve returned HTTP ",
		"Public Cache artifact returned HTTP ",
	} {
		index := strings.LastIndex(message, prefix)
		if index < 0 {
			continue
		}
		status, parseErr := strconv.Atoi(strings.TrimSpace(message[index+len(prefix):]))
		if parseErr == nil && (status == 408 || status == 429 || status >= 500 && status <= 599) {
			return true
		}
	}
	return false
}

func configureActionsIndex(
	cfg config.Config,
	local *actionscache.PersistentStorage,
	publicClient *remote.PublicClient,
) (actionscache.StorageIndex, *actionscache.PublicStorage, error) {
	var team actionscache.StorageIndex
	if cfg.Role == "local" && cfg.TeamURL != "" {
		remoteTeam, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
			Endpoint: cfg.TeamURL,
			Token:    cfg.TeamToken,
		})
		if err != nil {
			return nil, nil, err
		}
		team = remoteTeam
	}

	if cfg.Role != "local" || cfg.PublicURL == "" {
		if team == nil {
			return local, nil, nil
		}
		hierarchy, err := actionscache.NewCacheChain(local, team)
		return hierarchy, nil, err
	}
	verificationKey, err := publictrust.DecodePublicKey(cfg.PublicTrustKey)
	if err != nil {
		return nil, nil, err
	}
	public, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey:  verificationKey,
		StagingDirectory: filepath.Join(cfg.DataDir, "actions-public-staging"),
		ArtifactStore:    local.ArtifactStore(),
	}, actionsPublicResolver{client: publicClient})
	if err != nil {
		return nil, nil, err
	}
	hierarchy, err := actionscache.NewCacheChainWithPublic(local, team, public)
	if err != nil {
		_ = public.Close()
		return nil, nil, err
	}
	return hierarchy, public, nil
}
