package server

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/remote"
)

type actionsPublicResolver struct {
	client *remote.PublicClient
}

type offlineActionsPublicResolver struct{}

func (offlineActionsPublicResolver) Resolve(
	context.Context,
	actionscache.PublicResolveRequest,
) (actionscache.PublicResolution, error) {
	return actionscache.PublicResolution{}, actionscache.ErrPublicOffline
}

func (offlineActionsPublicResolver) Revalidate(
	context.Context,
	actionscache.PublicResolveRequest,
) (publictrust.Envelope, error) {
	return publictrust.Envelope{}, actionscache.ErrPublicOffline
}

// disabledActionsPublicCache makes a missing trust key authoritative. It is
// still installed around Local Cache so historical Public entries are
// invalidated instead of silently becoming ordinary unleased local entries.
type disabledActionsPublicCache struct{}

func (disabledActionsPublicCache) Lookup(
	context.Context,
	actionscache.LookupRequest,
) (actionscache.LookupResult, error) {
	return actionscache.LookupResult{}, actionscache.ErrNotFound
}

func (disabledActionsPublicCache) Open(
	context.Context,
	actionscache.OpenRequest,
) (actionscache.Archive, error) {
	return actionscache.Archive{}, actionscache.ErrNotFound
}

func (disabledActionsPublicCache) RevalidateEntry(
	context.Context,
	*actionscache.PublicEntryMetadata,
) (*actionscache.PublicEntryMetadata, error) {
	return nil, publictrust.ErrIdentity
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
	local actionscache.StorageIndex,
	staging *artifact.Store,
	publicClient *remote.PublicClient,
) (actionscache.StorageIndex, *actionscache.PublicCacheIndex, *actionscache.RemoteStorage, error) {
	var team *actionscache.RemoteStorage
	if cfg.Role == "local" && cfg.TeamURL != "" && cfg.TeamToken != "" {
		remoteTeam, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
			Endpoint: cfg.TeamURL,
			Token:    cfg.TeamToken,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		team = remoteTeam
	}
	var teamIndex actionscache.StorageIndex
	if team != nil {
		teamIndex = team
	}
	if cfg.Role != "local" {
		hierarchy, err := actionscache.NewCacheChainWithPublic(local, nil, disabledActionsPublicCache{})
		return hierarchy, nil, nil, err
	}

	if cfg.PublicURL == "" && cfg.PublicTrustKey == "" {
		hierarchy, err := actionscache.NewCacheChainWithPublic(local, teamIndex, disabledActionsPublicCache{})
		return hierarchy, nil, team, err
	}
	verificationKey, err := publictrust.DecodePublicKey(cfg.PublicTrustKey)
	if err != nil {
		return nil, nil, nil, err
	}
	resolver := actionscache.PublicResolver(actionsPublicResolver{client: publicClient})
	if cfg.PublicURL == "" {
		resolver = offlineActionsPublicResolver{}
	}
	public, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
		VerificationKey:    verificationKey,
		StagingDirectory:   filepath.Join(cfg.DataDir, "actions-public-staging"),
		ArtifactStore:      staging,
		RequireSafeArchive: true,
	}, resolver)
	if err != nil {
		return nil, nil, nil, err
	}
	hierarchy, err := actionscache.NewCacheChainWithPublic(local, teamIndex, public)
	if err != nil {
		_ = public.Close()
		return nil, nil, nil, err
	}
	return hierarchy, public, team, nil
}
