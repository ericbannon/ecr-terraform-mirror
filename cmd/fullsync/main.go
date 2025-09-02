package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
)

// ---------- small helpers ----------

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing required env: %s", k)
	}
	return v
}

func ecrHost(accountID, region string) string {
	return fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", accountID, region)
}

func ensureECRRepo(ctx context.Context, cli *ecr.Client, repo string) error {
	_, err := cli.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{repo},
	})
	if err == nil {
		return nil
	}
	_, err = cli.CreateRepository(ctx, &ecr.CreateRepositoryInput{
		RepositoryName: &repo,
		ImageScanningConfiguration: &ecrtypes.ImageScanningConfiguration{
			ScanOnPush: true,
		},
	})
	if err != nil && !strings.Contains(err.Error(), "RepositoryAlreadyExistsException") {
		return err
	}
	return nil
}

func ecrPassword(ctx context.Context, cli *ecr.Client, registryIDs []string) (string, error) {
	out, err := cli.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{
		RegistryIds: registryIDs,
	})
	if err != nil {
		return "", err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", fmt.Errorf("no ECR authorization data")
	}
	raw := *out.AuthorizationData[0].AuthorizationToken
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(string(dec), ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("unexpected ECR token format")
	}
	return parts[1], nil
}

type staticKeychain struct{ m map[string]authn.Authenticator }

func (s staticKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if a, ok := s.m[r.RegistryStr()]; ok {
		return a, nil
	}
	return authn.Anonymous, nil
}

func copyImage(src, dst string, kc authn.Keychain) error {
	if _, err := name.ParseReference(src); err != nil {
		return fmt.Errorf("parse src: %w", err)
	}
	if _, err := name.ParseReference(dst); err != nil {
		return fmt.Errorf("parse dst: %w", err)
	}
	return crane.Copy(src, dst, crane.WithAuthFromKeychain(kc))
}

// --- NEW: check if a tag already exists in ECR and return its digest ---
func ecrTagDigest(ctx context.Context, cli *ecr.Client, repo, tag string) (digest string, exists bool, err error) {
	out, err := cli.DescribeImages(ctx, &ecr.DescribeImagesInput{
		RepositoryName: &repo,
		ImageIds:       []ecrtypes.ImageIdentifier{{ImageTag: &tag}},
	})
	if err != nil {
		// If the repo was just created or not found, treat as not existing
		if strings.Contains(err.Error(), "RepositoryNotFoundException") {
			return "", false, nil
		}
		return "", false, err
	}
	if len(out.ImageDetails) == 0 || out.ImageDetails[0].ImageDigest == nil {
		return "", false, nil
	}
	return *out.ImageDetails[0].ImageDigest, true, nil
}

// ---------- lambda ----------

func handler(ctx context.Context) (string, error) {
	// Source registry (Chainguard)
	srcRegistry := os.Getenv("SRC_REGISTRY")
	if srcRegistry == "" {
		srcRegistry = "cgr.dev"
	}
	group := mustEnv("GROUP_NAME") // e.g. "bannon.dev"

	// Pull-token credentials for cgr.dev
	cgrUser := mustEnv("CGR_USERNAME")
	cgrPass := mustEnv("CGR_PASSWORD")

	// Optional ECR prefix (folder). Leave empty to mirror 1:1.
	dstPrefix := strings.TrimSuffix(os.Getenv("DST_PREFIX"), "/")

	// AWS/ECR setup
	awscfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("aws cfg: %w", err)
	}
	stsCli := sts.NewFromConfig(awscfg)
	who, err := stsCli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("sts: %w", err)
	}
	accountID := *who.Account
	region := awscfg.Region
	ecrCli := ecr.NewFromConfig(awscfg)
	ecrPass, err := ecrPassword(ctx, ecrCli, []string{accountID})
	if err != nil {
		return "", fmt.Errorf("ecr auth: %w", err)
	}
	dstHost := ecrHost(accountID, region)

	// Auth mapping: cgr.dev with pull token; ECR with AWS basic
	kc := staticKeychain{
		m: map[string]authn.Authenticator{
			srcRegistry: &authn.Basic{Username: cgrUser, Password: cgrPass},
			dstHost:     &authn.Basic{Username: "AWS", Password: ecrPass},
		},
	}

	// 1) List all repos from cgr.dev and filter to "<group>/..."
	repos, err := crane.Catalog(srcRegistry, crane.WithAuthFromKeychain(kc))
	if err != nil {
		return "", fmt.Errorf("catalog %s: %w", srcRegistry, err)
	}
	prefix := group + "/"
	var matched []string
	for _, r := range repos {
		if strings.HasPrefix(r, prefix) {
			matched = append(matched, r) // keep "bannon.dev/<repo>"
		}
	}
	log.Printf("found %d repos under %s/*", len(matched), group)

	// 2) For each repo: ensure ECR repo exists, list tags, copy each
	for _, repoPath := range matched {
		fullSrcRepo := srcRegistry + "/" + repoPath // e.g. cgr.dev/bannon.dev/alpine
		tags, err := crane.ListTags(fullSrcRepo, crane.WithAuthFromKeychain(kc))
		if err != nil {
			log.Printf("list tags failed for %s: %v", fullSrcRepo, err)
			continue
		}
		if len(tags) == 0 {
			continue
		}

		// Destination repo path in ECR (preserve structure, optional DST_PREFIX)
		dstRepo := repoPath
		if dstPrefix != "" {
			dstRepo = path.Join(dstPrefix, repoPath)
		}

		if err := ensureECRRepo(ctx, ecrCli, dstRepo); err != nil {
			log.Printf("ensure ECR repo %s: %v", dstRepo, err)
			continue
		}

		for _, tag := range tags {
			src := fmt.Sprintf("%s:%s", fullSrcRepo, tag)
			dst := fmt.Sprintf("%s/%s:%s", dstHost, dstRepo, tag)

			// --- NEW: precheck existing tag/digest on destination ---
			// 1) get source digest
			srcDigest, err := crane.Digest(src, crane.WithAuthFromKeychain(kc))
			if err != nil {
				log.Printf("digest src %s: %v (will attempt copy anyway)", src, err)
			}
			// 2) get destination digest (if tag exists)
			if dstDigest, exists, err := ecrTagDigest(ctx, ecrCli, dstRepo, tag); err != nil {
				log.Printf("check dst digest %s (repo=%s tag=%s): %v", dst, dstRepo, tag, err)
			} else if exists && srcDigest != "" && dstDigest == srcDigest {
				log.Printf("skip (exists, same digest) %s -> %s [%s]", src, dst, srcDigest)
				continue
			}

			log.Printf("copy %s -> %s", src, dst)
			if err := copyImage(src, dst, kc); err != nil {
				log.Printf("copy error %s -> %s: %v", src, dst, err)
			}
		}
	}

	return "ok", nil
}

func main() { lambda.Start(handler) }