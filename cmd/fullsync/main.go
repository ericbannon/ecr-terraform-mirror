// main.go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	ecrsvc "github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ---------- Event & config ----------

// JobEvent is the payload we pass when chaining.
type JobEvent struct {
	Index int    `json:"index,omitempty"`
	Repo  string `json:"repo,omitempty"`
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context, raw json.RawMessage) error {
	start := time.Now()

	evt, err := parseJobEvent(raw)
	if err != nil {
		log.Printf("WARN: event parse failed (%v); defaulting to index=0", err)
		evt = JobEvent{Index: 0}
	}

	repos, err := loadRepoList(ctx)
	if err != nil {
		return fmt.Errorf("load repo list: %w", err)
	}
	if len(repos) == 0 {
		log.Printf("No repositories to process; exiting.")
		return nil
	}

	// If a specific repo is provided, process exactly that and exit (no chaining).
	if r := strings.TrimSpace(evt.Repo); r != "" {
		log.Printf("Processing explicit repo: %s", r)
		if err := mirrorSingleRepo(ctx, r); err != nil {
			return fmt.Errorf("mirror explicit repo %q: %w", r, err)
		}
		log.Printf("Done explicit repo in %s", time.Since(start))
		return nil
	}

	// Index-bound checks.
	if evt.Index < 0 {
		evt.Index = 0
	}
	if evt.Index >= len(repos) {
		log.Printf("Index %d >= repo count %d; nothing to do.", evt.Index, len(repos))
		return nil
	}

	current := repos[evt.Index]
	log.Printf("Processing repo %d/%d: %s", evt.Index+1, len(repos), current)

	if err := mirrorSingleRepo(ctx, current); err != nil {
		return fmt.Errorf("mirror %q: %w", current, err)
	}

	// Chain to next index if any remain.
	next := evt.Index + 1
	if next < len(repos) {
		if err := invokeSelfAsync(ctx, JobEvent{Index: next}); err != nil {
			return fmt.Errorf("invoke self for index=%d: %w", next, err)
		}
		log.Printf("Queued next index=%d (elapsed %s)", next, time.Since(start))
	} else {
		log.Printf("Completed all %d repos 🎉 (elapsed %s)", len(repos), time.Since(start))
	}

	return nil
}

// ---------- Event parsing ----------

func parseJobEvent(raw json.RawMessage) (JobEvent, error) {
	var evt JobEvent
	if len(raw) == 0 {
		// No payload; allow START_INDEX env.
		idx := 0
		if s := strings.TrimSpace(os.Getenv("START_INDEX")); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				idx = n
			}
		}
		return JobEvent{Index: idx}, nil
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return JobEvent{}, err
	}
	return evt, nil
}

// ---------- Repo list discovery ----------

func loadRepoList(ctx context.Context) ([]string, error) {
	// Highest precedence: JSON env
	if s := strings.TrimSpace(os.Getenv("REPO_LIST_JSON")); s != "" {
		var repos []string
		if err := json.Unmarshal([]byte(s), &repos); err == nil {
			return normalizeRepoList(repos), nil
		}
		log.Printf("WARN: REPO_LIST_JSON invalid; falling back...")
	}

	// Next: CSV env
	if s := strings.TrimSpace(os.Getenv("REPO_LIST_CSV")); s != "" {
		return normalizeRepoList(strings.Split(s, ",")), nil
	}

	// Next: SSM parameter (JSON array or CSV)
	if name := strings.TrimSpace(os.Getenv("REPO_LIST_SSM_PARAM")); name != "" {
		repos, err := loadReposFromSSM(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("load from SSM %q: %w", name, err)
		}
		return repos, nil
	}

	// Fallback: minimal discovery (replace with real implementation if desired)
	group := os.Getenv("GROUP_NAME")
	if group == "" {
		group = "chainguard"
	}
	srcRegistry := os.Getenv("SRC_REGISTRY")
	if srcRegistry == "" {
		srcRegistry = "cgr.dev"
	}

	return []string{
		fmt.Sprintf("%s/%s/foo", srcRegistry, group),
		fmt.Sprintf("%s/%s/bar", srcRegistry, group),
		fmt.Sprintf("%s/%s/baz", srcRegistry, group),
	}, nil
}

func normalizeRepoList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func loadReposFromSSM(ctx context.Context, paramName string) ([]string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := ssm.NewFromConfig(cfg)
	withDecryption := true
	resp, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(paramName),
		WithDecryption: &withDecryption,
	})
	if err != nil {
		return nil, err
	}
	if resp.Parameter == nil || resp.Parameter.Value == nil {
		return nil, errors.New("empty SSM parameter")
	}
	val := strings.TrimSpace(*resp.Parameter.Value)

	// Try JSON first.
	var repos []string
	if json.Unmarshal([]byte(val), &repos) == nil {
		return normalizeRepoList(repos), nil
	}
	// Else treat as CSV.
	return normalizeRepoList(strings.Split(val, ",")), nil
}

// ---------- Mirroring logic (with "skip existing tag" optimization) ----------

func mirrorSingleRepo(ctx context.Context, srcRepo string) error {
	if strings.EqualFold(os.Getenv("MIRROR_DRY_RUN"), "true") {
		log.Printf("[DRY-RUN] Would mirror: %s", srcRepo)
		return nil
	}

	// 1) Source auth (cgr.dev)
	srcUser := os.Getenv("CGR_USERNAME")
	srcPass := os.Getenv("CGR_PASSWORD")
	if srcUser == "" || srcPass == "" {
		return fmt.Errorf("CGR_USERNAME/CGR_PASSWORD not set")
	}
	srcAuth := &authn.Basic{Username: srcUser, Password: srcPass}

	// 2) ECR auth + endpoint
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	ecr := ecrsvc.NewFromConfig(cfg)

	ao, err := ecr.GetAuthorizationToken(ctx, &ecrsvc.GetAuthorizationTokenInput{})
	if err != nil {
		return fmt.Errorf("ecr:GetAuthorizationToken: %w", err)
	}
	if len(ao.AuthorizationData) == 0 {
		return fmt.Errorf("no ECR authorization data")
	}
	ad := ao.AuthorizationData[0]
	dec, err := base64.StdEncoding.DecodeString(aws.ToString(ad.AuthorizationToken))
	if err != nil {
		return fmt.Errorf("decode ecr token: %w", err)
	}
	parts := strings.SplitN(string(dec), ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("unexpected ecr token format")
	}
	ecrPass := parts[1]
	ecrEndpoint := strings.TrimPrefix(aws.ToString(ad.ProxyEndpoint), "https://")
	dstAuth := &authn.Basic{Username: "AWS", Password: ecrPass}

	// 3) Compute dest repo name; ensure it exists
	srcRegistry := os.Getenv("SRC_REGISTRY")
	if srcRegistry == "" {
		srcRegistry = "cgr.dev"
	}
	srcNoHost := strings.TrimPrefix(srcRepo, srcRegistry+"/")

	dstPrefix := strings.Trim(strings.TrimPrefix(os.Getenv("DST_PREFIX"), "/"), " ")
	var dstRepoName string
	if dstPrefix != "" {
		dstRepoName = path.Join(dstPrefix, srcNoHost)
	} else {
		dstRepoName = srcNoHost
	}

	_, err = ecr.DescribeRepositories(ctx, &ecrsvc.DescribeRepositoriesInput{
		RepositoryNames: []string{dstRepoName},
	})
	if err != nil {
		var rnfe *ecrtypes.RepositoryNotFoundException
		if errors.As(err, &rnfe) {
			_, cerr := ecr.CreateRepository(ctx, &ecrsvc.CreateRepositoryInput{
				RepositoryName: aws.String(dstRepoName),
				ImageScanningConfiguration: &ecrtypes.ImageScanningConfiguration{
					// NOTE: in your SDK version this field is a bool (not *bool)
					ScanOnPush: true,
				},
			})
			if cerr != nil {
				return fmt.Errorf("create ECR repo %s: %w", dstRepoName, cerr)
			}
			log.Printf("Created ECR repo: %s", dstRepoName)
		} else {
			return fmt.Errorf("describe ECR repo %s: %w", dstRepoName, err)
		}
	}

	// 4) Decide which tags to mirror
	copyAll := strings.EqualFold(os.Getenv("COPY_ALL_TAGS"), "true")
	var tags []string
	if copyAll {
		repoRef, err := name.NewRepository(srcRepo)
		if err != nil {
			return fmt.Errorf("parse src repository: %w", err)
		}
		tags, err = remote.List(repoRef, remote.WithAuth(srcAuth), remote.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("list tags for %s: %w", srcRepo, err)
		}
		if len(tags) == 0 {
			log.Printf("No tags found for %s", srcRepo)
			return nil
		}
	} else {
		tags = []string{"latest"} // fast smoke test
	}

	// 5) For each tag: compare digests and skip if already identical in ECR
	for _, tag := range tags {
		src := fmt.Sprintf("%s:%s", srcRepo, tag)
		dst := fmt.Sprintf("%s/%s:%s", ecrEndpoint, dstRepoName, tag)

		srcRef, err := name.ParseReference(src)
		if err != nil {
			return fmt.Errorf("parse src ref %s: %w", src, err)
		}
		dstRef, err := name.ParseReference(dst)
		if err != nil {
			return fmt.Errorf("parse dst ref %s: %w", dst, err)
		}

		// Get source descriptor (to obtain source digest)
		desc, err := remote.Get(srcRef, remote.WithAuth(srcAuth), remote.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("get %s: %w", src, err)
		}
		srcDigest := desc.Descriptor.Digest.String()

		// Query ECR for current tag digest (if present)
		ebr, err := ecr.BatchGetImage(ctx, &ecrsvc.BatchGetImageInput{
			RepositoryName: aws.String(dstRepoName),
			ImageIds: []ecrtypes.ImageIdentifier{
				{ImageTag: aws.String(tag)},
			},
		})
		if err == nil && len(ebr.Images) > 0 && ebr.Images[0].ImageId != nil && ebr.Images[0].ImageId.ImageDigest != nil {
			existing := aws.ToString(ebr.Images[0].ImageId.ImageDigest)
			if existing == srcDigest {
				log.Printf("Skipping %s (tag %q) — already present with digest %s", srcRepo, tag, srcDigest)
				continue
			}
		}

		// Copy image/index since it's missing or digest differs
		if desc.MediaType.IsIndex() {
			idx, err := desc.ImageIndex()
			if err != nil {
				return fmt.Errorf("read index %s: %w", src, err)
			}
			log.Printf("Copying index %s -> %s", src, dst)
			if err := remote.WriteIndex(dstRef, idx, remote.WithAuth(dstAuth), remote.WithContext(ctx)); err != nil {
				return fmt.Errorf("write index to %s: %w", dst, err)
			}
		} else {
			img, err := desc.Image()
			if err != nil {
				return fmt.Errorf("read image %s: %w", src, err)
			}
			log.Printf("Copying image %s -> %s", src, dst)
			if err := remote.Write(dstRef, img, remote.WithAuth(dstAuth), remote.WithContext(ctx)); err != nil {
				return fmt.Errorf("write image to %s: %w", dst, err)
			}
		}
	}

	log.Printf("Mirrored %s (%d tag(s) considered)", srcRepo, len(tags))
	return nil
}

// ---------- Self-invocation ----------

func invokeSelfAsync(ctx context.Context, ev JobEvent) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	client := lambdasvc.NewFromConfig(cfg)

	fn := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	if fn == "" {
		return errors.New("AWS_LAMBDA_FUNCTION_NAME not set")
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	_, err = client.Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(fn),
		InvocationType: lambdatypes.InvocationTypeEvent, // async
		Payload:        payload,
	})
	return err
}