//go:build ignore

// 一次性清理 GHCR 中残留的无 tag 镜像版本。
//
// 判定规则（宁可不删，也不误删）：
//   - 有 tag 的 version 永远不动（latest / v* / sha-* / snapshot / Snapshot-*）；
//   - 无 tag 的 version，只有确认它没有被任何"有 tag 的索引"引用时才删；
//   - 拿不到某个索引的子 manifest 列表时直接中止，不猜。
//
// 用法：
//
//	go run scripts/prune-ghcr.go -repo huilang-me/cfsm-agent           # dry-run，只打印
//	go run scripts/prune-ghcr.go -repo huilang-me/cfsm-agent -apply    # 真正删除
//
// token 依次取环境变量 GH_TOKEN、GITHUB_TOKEN，都没有时执行 `gh auth token`。
// 私有包需要带 read:packages 权限；公开包用匿名 token 即可。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	apiBase      = "https://api.github.com"
	registryBase = "https://ghcr.io"
	perPage      = 100
)

type version struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"` // 镜像 digest
	CreatedAt time.Time `json:"created_at"`
	Metadata  struct {
		Container struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

func (v version) tags() []string { return v.Metadata.Container.Tags }

type manifestIndex struct {
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.msg) }

func isStatus(err error, code int) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == code
}

func main() {
	repo := flag.String("repo", "", "owner/repo，例如 huilang-me/cfsm-agent")
	pkgName := flag.String("package", "", "GHCR 包名，默认取 repo 名")
	apply := flag.Bool("apply", false, "真正执行删除；不加则只打印")
	flag.Parse()

	if err := run(*repo, *pkgName, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		if isStatus(err, http.StatusForbidden) {
			fmt.Fprintln(os.Stderr, "提示: token 缺少 packages 权限。先执行 `gh auth refresh -s read:packages,delete:packages`，")
			fmt.Fprintln(os.Stderr, "      或改用带该权限的 PAT：GH_TOKEN=ghp_xxx go run scripts/prune-ghcr.go -repo ...")
			fmt.Fprintln(os.Stderr, "      注意 shell 里已有的 GH_TOKEN / GITHUB_TOKEN 会被优先使用，必要时先 unset。")
		}
		os.Exit(1)
	}
}

func run(repo, pkgName string, apply bool) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return errors.New("-repo 需要 owner/repo 形式")
	}
	if pkgName == "" {
		pkgName = name
	}
	pkg := strings.ToLower(pkgName)
	token := githubToken()

	scopePath, versions, err := listVersions(owner, pkg, token)
	if err != nil {
		return err
	}

	reg := &registry{client: &http.Client{Timeout: 30 * time.Second}, owner: owner, pkg: pkg, token: token}

	// 所有"有 tag"的 version 都是在用镜像，它们的子 manifest 必须保留。
	tagged := 0
	referenced := map[string]bool{}
	for _, v := range versions {
		if len(v.tags()) == 0 {
			continue
		}
		tagged++
		children, err := reg.childDigests(v.Name)
		if err != nil {
			return fmt.Errorf("无法读取 %s 的子 manifest，为安全起见中止: %w", v.Name, err)
		}
		for _, child := range children {
			referenced[child] = true
		}
	}

	var candidates []version
	for _, v := range versions {
		if len(v.tags()) > 0 || referenced[v.Name] {
			continue
		}
		candidates = append(candidates, v)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CreatedAt.Before(candidates[j].CreatedAt) })

	fmt.Printf("包: ghcr.io/%s/%s\n", owner, pkg)
	fmt.Printf("版本总数 %d：有 tag %d，无 tag %d\n", len(versions), tagged, len(versions)-tagged)
	fmt.Printf("被在用索引引用、需保留的无 tag 子 manifest %d 个\n", len(referenced))
	fmt.Printf("可清理的无 tag 版本 %d 个\n\n", len(candidates))
	if len(candidates) == 0 {
		fmt.Println("没有需要清理的版本。")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCREATED\tDIGEST")
	for _, v := range candidates {
		fmt.Fprintf(w, "%d\t%s\t%s\n", v.ID, v.CreatedAt.UTC().Format("2006-01-02 15:04:05"), v.Name)
	}
	_ = w.Flush()

	if !apply {
		fmt.Printf("\ndry-run：未删除任何内容。确认列表无误后加 -apply 重跑。\n")
		return nil
	}

	fmt.Println()
	failed := 0
	for _, v := range candidates {
		if err := deleteVersion(scopePath, token, v.ID); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "删除 %d (%s) 失败: %v\n", v.ID, v.Name, err)
			continue
		}
		fmt.Printf("已删除 %d (%s)\n", v.ID, v.Name)
	}
	if failed > 0 {
		return fmt.Errorf("%d 个版本删除失败", failed)
	}
	return nil
}

func githubToken() string {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// listVersions 兼容 user 和 org 两种归属，返回实际可用的路径前缀。
func listVersions(owner, pkg, token string) (string, []version, error) {
	var lastErr error
	for _, scope := range []string{"users", "orgs"} {
		path := fmt.Sprintf("/%s/%s/packages/container/%s/versions", scope, owner, pkg)
		versions, err := fetchVersions(path, token)
		if err == nil {
			return path, versions, nil
		}
		lastErr = err
		if !isStatus(err, http.StatusNotFound) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("找不到容器包 %s/%s: %w", owner, pkg, lastErr)
}

func fetchVersions(path, token string) ([]version, error) {
	var all []version
	for page := 1; ; page++ {
		u := fmt.Sprintf("%s%s?per_page=%d&page=%d", apiBase, path, perPage, page)
		var batch []version
		if err := getJSON(u, token, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < perPage {
			return all, nil
		}
	}
}

func getJSON(u, token string, out any) error {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{code: resp.StatusCode, msg: strings.TrimSpace(string(body))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func deleteVersion(scopePath, token string, id int64) error {
	u := fmt.Sprintf("%s%s/%d", apiBase, scopePath, id)
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &statusError{code: resp.StatusCode, msg: strings.TrimSpace(string(body))}
}

type registry struct {
	client *http.Client
	owner  string
	pkg    string
	token  string

	anonBearer   string
	authedBearer string
}

// childDigests 返回该 digest 对应索引下的子 manifest；单架构镜像返回空。
// 匿名 token 读不到时（私有包）自动改用带 read:packages 的 GitHub token 重试一次。
func (r *registry) childDigests(digest string) ([]string, error) {
	children, err := r.manifestChildren(digest, false)
	if err != nil && (isStatus(err, http.StatusUnauthorized) || isStatus(err, http.StatusForbidden)) && r.token != "" {
		return r.manifestChildren(digest, true)
	}
	return children, err
}

func (r *registry) manifestChildren(digest string, authed bool) ([]string, error) {
	bearer, err := r.bearer(authed)
	if err != nil {
		return nil, err
	}
	u := registryBase + "/v2/" + r.owner + "/" + r.pkg + "/manifests/" + digest
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, &statusError{code: resp.StatusCode, msg: strings.TrimSpace(string(body))}
	}
	var idx manifestIndex
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(idx.Manifests))
	for _, m := range idx.Manifests {
		out = append(out, m.Digest)
	}
	return out, nil
}

func (r *registry) bearer(authed bool) (string, error) {
	if authed && r.authedBearer != "" {
		return r.authedBearer, nil
	}
	if !authed && r.anonBearer != "" {
		return r.anonBearer, nil
	}
	scope := url.QueryEscape("repository:" + r.owner + "/" + r.pkg + ":pull")
	req, err := http.NewRequest(http.MethodGet, registryBase+"/token?scope="+scope, nil)
	if err != nil {
		return "", err
	}
	if authed {
		req.SetBasicAuth(r.owner, r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &statusError{code: resp.StatusCode, msg: "GHCR token 接口"}
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.Token == "" {
		return "", errors.New("GHCR 返回空 token")
	}
	if authed {
		r.authedBearer = payload.Token
	} else {
		r.anonBearer = payload.Token
	}
	return payload.Token, nil
}
