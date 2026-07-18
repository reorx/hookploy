package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// imagePin implements the image.pin contract (PRD §4 模式 op 语义):
//  1. use the rollout digest, or resolve :latest's RepoDigest when absent;
//  2. docker pull <image>@<digest> with retries;
//  3. re-point the local :latest tag at the pinned image;
//  4. remember the pinned image id for the post-deploy verification.
func (e *Engine) imagePin(ctx context.Context, spec Spec, idx int, st *execState, sink Sink) (*int, error) {
	if spec.Image == "" {
		return nil, fmt.Errorf("image.pin: service has no \"image\" declared")
	}
	digest := spec.Digest
	if digest == "" {
		// manual trigger: pull :latest and resolve its actual digest
		latest := spec.Image + ":latest"
		if exit, err := e.runCmd(ctx, spec, idx, sink, []string{"docker", "pull", latest}); err != nil {
			return exit, fmt.Errorf("pull %s: %w", latest, err)
		}
		out, err := e.runCapture(ctx, spec, idx, sink,
			[]string{"docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", latest}, "stdout")
		if err != nil {
			return nil, fmt.Errorf("resolve digest of %s: %w", latest, err)
		}
		_, d, found := strings.Cut(out, "@")
		if !found || !digestRe.MatchString(d) {
			return nil, fmt.Errorf("cannot parse digest from RepoDigests %q", out)
		}
		digest = d
	} else if !digestRe.MatchString(digest) {
		return nil, fmt.Errorf("invalid digest %q", digest)
	}

	pinned := spec.Image + "@" + digest
	var lastExit *int
	var lastErr error
	for attempt := 1; attempt <= e.pullRetries(); attempt++ {
		lastExit, lastErr = e.runCmd(ctx, spec, idx, sink, []string{"docker", "pull", pinned})
		if lastErr == nil {
			break
		}
		if attempt < e.pullRetries() {
			sink.Log(idx, "system", fmt.Sprintf("pull attempt %d/%d failed, retrying: %v\n", attempt, e.pullRetries(), lastErr))
			if err := e.sleep(ctx, e.pullInterval()); err != nil {
				return lastExit, err
			}
		}
	}
	if lastErr != nil {
		return lastExit, fmt.Errorf("pull %s failed after %d attempts: %w", pinned, e.pullRetries(), lastErr)
	}

	id, err := e.runCapture(ctx, spec, idx, sink,
		[]string{"docker", "image", "inspect", "--format", "{{.Id}}", pinned}, "stdout")
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", pinned, err)
	}
	if id == "" {
		return nil, fmt.Errorf("inspect %s returned no image id", pinned)
	}
	if exit, err := e.runCmd(ctx, spec, idx, sink,
		[]string{"docker", "tag", pinned, spec.Image + ":latest"}); err != nil {
		return exit, fmt.Errorf("tag: %w", err)
	}
	st.pinnedImageID = id
	st.digest = digest
	zero := 0
	return &zero, nil
}

// verifyPin runs after the pipeline's last compose.up: at least one running
// compose container must use the pinned image id. It is not an explicit op;
// its output goes to the system stream of the compose.up op.
func (e *Engine) verifyPin(ctx context.Context, spec Spec, idx int, st *execState, sink Sink) error {
	sink.Log(idx, "system", fmt.Sprintf("verifying deployment uses pinned image %s\n", st.pinnedImageID))
	out, err := e.runCapture(ctx, spec, idx, sink, []string{"docker", "compose", "ps", "-q"}, "system")
	if err != nil {
		return fmt.Errorf("image.pin verification: compose ps: %w", err)
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return fmt.Errorf("image.pin verification failed: no running containers")
	}
	for _, cid := range ids {
		img, err := e.runCapture(ctx, spec, idx, sink,
			[]string{"docker", "inspect", "--format", "{{.Image}}", cid}, "system")
		if err != nil {
			return fmt.Errorf("image.pin verification: inspect %s: %w", cid, err)
		}
		if img == st.pinnedImageID {
			sink.Log(idx, "system", fmt.Sprintf("verification OK: container %s runs %s\n", cid, img))
			return nil
		}
	}
	return fmt.Errorf("image.pin verification failed: no container runs pinned image %s", st.pinnedImageID)
}
