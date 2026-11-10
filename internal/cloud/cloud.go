package cloud

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amglogicalis/permafrost/internal/crypto"
	"github.com/amglogicalis/permafrost/internal/vault"
)

// Bundle encapsulates a complete snapshot Manifest along with its deduplicated compressed blobs.
type Bundle struct {
	Version   string              `json:"version"`
	Manifest  *vault.Manifest     `json:"manifest"`
	Blobs     map[string][]byte   `json:"blobs"` // hash -> compressed zstd/zlib bytes
	CreatedAt time.Time           `json:"created_at"`
}

// ChunkMeta describes a single split asset (<2GB) within a Cloud Release.
type ChunkMeta struct {
	Name   string `json:"name"`
	Index  int    `json:"index"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BundleIndex is stored as bundle_index.json in a release or S3 prefix to guide reconstruction.
type BundleIndex struct {
	ReleaseKey  string      `json:"release_key"`
	ProjectID   string      `json:"project_id"`
	ProjectName string      `json:"project_name"`
	SnapshotID  string      `json:"snapshot_id"`
	Tag         string      `json:"tag"`
	Message     string      `json:"message"`
	Encrypted   bool        `json:"encrypted"`
	TotalBytes  int64       `json:"total_bytes"`
	ChunkSize   int64       `json:"chunk_size"`
	Chunks      []ChunkMeta `json:"chunks"`
	CreatedAt   time.Time   `json:"created_at"`
}

// Provider defines the interface implemented by GitHub Releases, S3/R2, and Folder backends.
type Provider interface {
	Name() string
	UploadRelease(index *BundleIndex, chunks map[string][]byte) error
	DownloadRelease(releaseKey string) (*BundleIndex, map[string][]byte, error)
	ListReleases() ([]*BundleIndex, error)
}

// Manager coordinates packaging, Zero-Knowledge encryption, chunking, and cloud synchronization.
type Manager struct {
	Vault *vault.Vault
}

// New creates a Cloud & Teleport Manager backed by the central Vault.
func New(v *vault.Vault) *Manager {
	return &Manager{Vault: v}
}

// PackToChunks serializes a snapshot and its blobs, optionally encrypts with Zero-Knowledge AES-256-GCM,
// and splits the payload into chunks of at most maxChunkBytes.
func (m *Manager) PackToChunks(manifest *vault.Manifest, passphrase string, maxChunkBytes int64) (*BundleIndex, map[string][]byte, error) {
	if maxChunkBytes <= 0 {
		limitMB := m.Vault.Config.Cloud.ChunkLimitMB
		if limitMB <= 0 {
			limitMB = 100
		}
		maxChunkBytes = int64(limitMB) * 1024 * 1024
	}

	blobs := make(map[string][]byte)
	for _, f := range manifest.Files {
		for _, c := range f.Chunks {
			if _, ok := blobs[c.Hash]; ok {
				continue
			}
			bData, err := m.Vault.GetBlob(c.Hash)
			if err != nil {
				return nil, nil, fmt.Errorf("leyendo bloque %s para empaquetado cloud: %w", c.Hash, err)
			}
			blobs[c.Hash] = bData
		}
	}

	bundle := &Bundle{
		Version:   "1.0",
		Manifest:  manifest,
		Blobs:     blobs,
		CreatedAt: time.Now().UTC(),
	}

	rawBytes, err := json.Marshal(bundle)
	if err != nil {
		return nil, nil, err
	}

	encrypted := false
	payload := rawBytes
	if m.Vault.Config.Features.ZeroKnowledgeEncryption && strings.TrimSpace(passphrase) != "" {
		payload, err = crypto.Encrypt(rawBytes, passphrase)
		if err != nil {
			return nil, nil, err
		}
		encrypted = true
	}

	chunks := make(map[string][]byte)
	var metas []ChunkMeta
	totalLen := int64(len(payload))

	var offset int64
	idx := 1
	for offset < totalLen || (totalLen == 0 && idx == 1) {
		end := offset + maxChunkBytes
		if end > totalLen {
			end = totalLen
		}
		part := payload[offset:end]
		sum := sha256.Sum256(part)
		name := fmt.Sprintf("part_%03d.pfchunk", idx)

		partCopy := make([]byte, len(part))
		copy(partCopy, part)
		chunks[name] = partCopy

		metas = append(metas, ChunkMeta{
			Name:   name,
			Index:  idx,
			Size:   int64(len(partCopy)),
			SHA256: hex.EncodeToString(sum[:]),
		})

		offset = end
		idx++
		if totalLen == 0 {
			break
		}
	}

	relKey := fmt.Sprintf("pfrost-%s-%s", sanitizeReleaseKey(manifest.ProjectID), sanitizeReleaseKey(manifest.Tag))
	index := &BundleIndex{
		ReleaseKey:  relKey,
		ProjectID:   manifest.ProjectID,
		ProjectName: manifest.ProjectName,
		SnapshotID:  manifest.ID,
		Tag:         manifest.Tag,
		Message:     manifest.Message,
		Encrypted:   encrypted,
		TotalBytes:  totalLen,
		ChunkSize:   maxChunkBytes,
		Chunks:      metas,
		CreatedAt:   time.Now().UTC(),
	}

	return index, chunks, nil
}

// UnpackFromChunks verifies chunk SHA-256 digests, reassembles the payload, decrypts if Zero-Knowledge encrypted,
// and imports all blobs and the Manifest into the local Vault.
func (m *Manager) UnpackFromChunks(index *BundleIndex, chunks map[string][]byte, passphrase string, targetProjectPath string) (*vault.Manifest, error) {
	sortedMetas := make([]ChunkMeta, len(index.Chunks))
	copy(sortedMetas, index.Chunks)
	sort.Slice(sortedMetas, func(i, j int) bool {
		return sortedMetas[i].Index < sortedMetas[j].Index
	})

	var combined bytes.Buffer
	combined.Grow(int(index.TotalBytes))

	for _, meta := range sortedMetas {
		part, ok := chunks[meta.Name]
		if !ok {
			return nil, fmt.Errorf("falta el fragmento %s del paquete cloud", meta.Name)
		}
		sum := sha256.Sum256(part)
		gotHash := hex.EncodeToString(sum[:])
		if gotHash != meta.SHA256 {
			return nil, fmt.Errorf("checksum SHA-256 inválido en fragmento %s (esperado %s, obtenido %s)", meta.Name, meta.SHA256, gotHash)
		}
		combined.Write(part)
	}

	rawPayload := combined.Bytes()
	if index.Encrypted || crypto.IsEncrypted(rawPayload) {
		var err error
		rawPayload, err = crypto.Decrypt(rawPayload, passphrase)
		if err != nil {
			return nil, err
		}
	}

	var bundle Bundle
	if err := json.Unmarshal(rawPayload, &bundle); err != nil {
		return nil, fmt.Errorf("decodificando paquete Permafrost: %w", err)
	}
	if bundle.Manifest == nil {
		return nil, fmt.Errorf("el paquete no contiene un manifiesto válido")
	}

	// Import all deduplicated blobs into the local Vault
	for hash, compData := range bundle.Blobs {
		if exists, _ := m.Vault.HasBlob(hash); !exists {
			if err := m.Vault.PutBlob(hash, compData); err != nil {
				return nil, err
			}
		}
	}

	manifest := bundle.Manifest
	if strings.TrimSpace(targetProjectPath) != "" {
		proj, err := m.Vault.RegisterOrGetProject(targetProjectPath, manifest.ProjectName)
		if err != nil {
			return nil, err
		}
		manifest.ProjectID = proj.ID
		manifest.RootPath = proj.Path
	} else {
		_, _ = m.Vault.RegisterOrGetProject(manifest.RootPath, manifest.ProjectName)
	}

	if err := m.Vault.SaveManifest(manifest); err != nil {
		return nil, err
	}

	m.Vault.AppendAuditLog("CLOUD_BUNDLE_IMPORTED", fmt.Sprintf("Release=%s Tag=%s Encrypted=%v Chunks=%d",
		index.ReleaseKey, manifest.Tag, index.Encrypted, len(index.Chunks)))

	return manifest, nil
}

// ============================================================================
// Driver 1: GitHub Releases Provider (with Chunked Assets <2GB)
// ============================================================================

type GitHubReleasesProvider struct {
	Repo          string // owner/repo
	Token         string
	APIBaseURL    string
	UploadBaseURL string
	Client        *http.Client
}

func NewGitHubReleasesProvider(repo, token string) *GitHubReleasesProvider {
	if strings.TrimSpace(token) == "" {
		token = ResolveGitHubToken()
	}
	return &GitHubReleasesProvider{
		Repo:          strings.TrimSpace(repo),
		Token:         strings.TrimSpace(token),
		APIBaseURL:    "https://api.github.com",
		UploadBaseURL: "https://uploads.github.com",
		Client:        &http.Client{Timeout: 60 * time.Second},
	}
}

func (g *GitHubReleasesProvider) Name() string {
	return fmt.Sprintf("GitHub Releases (%s)", g.Repo)
}

func ResolveGitHubToken() string {
	for _, env := range []string{"PFROST_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

type ghReleaseResponse struct {
	ID        int64          `json:"id"`
	TagName   string         `json:"tag_name"`
	Name      string         `json:"name"`
	UploadURL string         `json:"upload_url"`
	Assets    []ghAssetEntry `json:"assets"`
}

type ghAssetEntry struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	URL                string `json:"url"`
}

func (g *GitHubReleasesProvider) UploadRelease(index *BundleIndex, chunks map[string][]byte) error {
	if g.Repo == "" {
		return fmt.Errorf("repositorio GitHub no configurado (usa 'pfrost cloud setup github <usuario/repo>')")
	}

	// 1. Create or fetch Release by tag
	rel, err := g.createOrGetRelease(index)
	if err != nil {
		return err
	}

	// 2. Upload bundle_index.json first
	indexBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := g.uploadAsset(rel.ID, "bundle_index.json", indexBytes); err != nil {
		return err
	}

	// 3. Upload each chunk asset
	for _, meta := range index.Chunks {
		data := chunks[meta.Name]
		if err := g.uploadAsset(rel.ID, meta.Name, data); err != nil {
			return err
		}
	}
	return nil
}

func (g *GitHubReleasesProvider) DownloadRelease(releaseKey string) (*BundleIndex, map[string][]byte, error) {
	relURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimSuffix(g.APIBaseURL, "/"), g.Repo, url.PathEscape(releaseKey))
	req, err := http.NewRequest("GET", relURL, nil)
	if err != nil {
		return nil, nil, err
	}
	g.setHeaders(req, "application/vnd.github+json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("GitHub API devolvió %d buscando release %s: %s", resp.StatusCode, releaseKey, string(body))
	}

	var rel ghReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, nil, err
	}

	assetMap := make(map[string]ghAssetEntry)
	for _, a := range rel.Assets {
		assetMap[a.Name] = a
	}

	idxAsset, ok := assetMap["bundle_index.json"]
	if !ok {
		return nil, nil, fmt.Errorf("la release %s no contiene bundle_index.json", releaseKey)
	}
	idxBytes, err := g.downloadAsset(idxAsset)
	if err != nil {
		return nil, nil, err
	}
	var index BundleIndex
	if err := json.Unmarshal(idxBytes, &index); err != nil {
		return nil, nil, err
	}

	chunks := make(map[string][]byte, len(index.Chunks))
	for _, meta := range index.Chunks {
		a, exists := assetMap[meta.Name]
		if !exists {
			return nil, nil, fmt.Errorf("activo faltante %s en GitHub Release %s", meta.Name, releaseKey)
		}
		cBytes, err := g.downloadAsset(a)
		if err != nil {
			return nil, nil, err
		}
		chunks[meta.Name] = cBytes
	}

	return &index, chunks, nil
}

func (g *GitHubReleasesProvider) ListReleases() ([]*BundleIndex, error) {
	relURL := fmt.Sprintf("%s/repos/%s/releases?per_page=30", strings.TrimSuffix(g.APIBaseURL, "/"), g.Repo)
	req, err := http.NewRequest("GET", relURL, nil)
	if err != nil {
		return nil, err
	}
	g.setHeaders(req, "application/vnd.github+json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API devolvió %d al listar releases", resp.StatusCode)
	}

	var releases []ghReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}

	var out []*BundleIndex
	for _, rel := range releases {
		for _, a := range rel.Assets {
			if a.Name == "bundle_index.json" {
				if b, err := g.downloadAsset(a); err == nil {
					var idx BundleIndex
					if json.Unmarshal(b, &idx) == nil {
						out = append(out, &idx)
					}
				}
			}
		}
	}
	return out, nil
}

func (g *GitHubReleasesProvider) createOrGetRelease(index *BundleIndex) (*ghReleaseResponse, error) {
	// Check if tag release already exists
	getURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimSuffix(g.APIBaseURL, "/"), g.Repo, url.PathEscape(index.ReleaseKey))
	reqGet, _ := http.NewRequest("GET", getURL, nil)
	g.setHeaders(reqGet, "application/vnd.github+json")
	if resp, err := g.Client.Do(reqGet); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var existing ghReleaseResponse
			if json.NewDecoder(resp.Body).Decode(&existing) == nil {
				return &existing, nil
			}
		}
	}

	createURL := fmt.Sprintf("%s/repos/%s/releases", strings.TrimSuffix(g.APIBaseURL, "/"), g.Repo)
	payload := map[string]interface{}{
		"tag_name":   index.ReleaseKey,
		"name":       fmt.Sprintf("Permafrost Snapshot: %s (%s)", index.ProjectName, index.Tag),
		"body":       fmt.Sprintf("Permafrost Cloud Snapshot\nProject: %s\nTag: %s\nEncrypted: %v\nChunks: %d", index.ProjectName, index.Tag, index.Encrypted, len(index.Chunks)),
		"draft":      false,
		"prerelease": false,
	}
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", createURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	g.setHeaders(req, "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("error creando GitHub Release (%d): %s", resp.StatusCode, string(b))
	}

	var created ghReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, err
	}
	return &created, nil
}

func (g *GitHubReleasesProvider) uploadAsset(releaseID int64, name string, data []byte) error {
	upURL := fmt.Sprintf("%s/repos/%s/releases/%d/assets?name=%s",
		strings.TrimSuffix(g.UploadBaseURL, "/"), g.Repo, releaseID, url.QueryEscape(name))
	req, err := http.NewRequest("POST", upURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	g.setHeaders(req, "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))

	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("error subiendo asset %s (%d): %s", name, resp.StatusCode, string(b))
	}
	return nil
}

func (g *GitHubReleasesProvider) downloadAsset(asset ghAssetEntry) ([]byte, error) {
	dlURL := asset.URL
	if dlURL == "" {
		dlURL = asset.BrowserDownloadURL
	}
	req, err := http.NewRequest("GET", dlURL, nil)
	if err != nil {
		return nil, err
	}
	g.setHeaders(req, "application/octet-stream")

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error descargando asset %s (%d)", asset.Name, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (g *GitHubReleasesProvider) setHeaders(req *http.Request, accept string) {
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// ============================================================================
// Driver 2: S3 / Cloudflare R2 / MinIO Provider (AWS Signature V4)
// ============================================================================

type S3CompatibleProvider struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	Client    *http.Client
}

func NewS3CompatibleProvider(endpoint, bucket, region string) *S3CompatibleProvider {
	if region == "" {
		region = "auto"
	}
	return &S3CompatibleProvider{
		Endpoint:  strings.TrimSuffix(endpoint, "/"),
		Bucket:    bucket,
		Region:    region,
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		Client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *S3CompatibleProvider) Name() string {
	return fmt.Sprintf("S3/R2 (%s/%s)", s.Endpoint, s.Bucket)
}

func (s *S3CompatibleProvider) UploadRelease(index *BundleIndex, chunks map[string][]byte) error {
	idxBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := s.putObject(fmt.Sprintf("%s/bundle_index.json", index.ReleaseKey), idxBytes); err != nil {
		return err
	}
	for _, meta := range index.Chunks {
		if err := s.putObject(fmt.Sprintf("%s/%s", index.ReleaseKey, meta.Name), chunks[meta.Name]); err != nil {
			return err
		}
	}
	return nil
}

func (s *S3CompatibleProvider) DownloadRelease(releaseKey string) (*BundleIndex, map[string][]byte, error) {
	idxBytes, err := s.getObject(fmt.Sprintf("%s/bundle_index.json", releaseKey))
	if err != nil {
		return nil, nil, err
	}
	var index BundleIndex
	if err := json.Unmarshal(idxBytes, &index); err != nil {
		return nil, nil, err
	}
	chunks := make(map[string][]byte, len(index.Chunks))
	for _, meta := range index.Chunks {
		b, err := s.getObject(fmt.Sprintf("%s/%s", releaseKey, meta.Name))
		if err != nil {
			return nil, nil, err
		}
		chunks[meta.Name] = b
	}
	return &index, chunks, nil
}

func (s *S3CompatibleProvider) ListReleases() ([]*BundleIndex, error) {
	return nil, nil
}

func (s *S3CompatibleProvider) putObject(key string, data []byte) error {
	objURL := fmt.Sprintf("%s/%s/%s", s.Endpoint, s.Bucket, key)
	req, err := http.NewRequest("PUT", objURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	s.signRequestV4(req, data)
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 PUT %s devolvió %d: %s", key, resp.StatusCode, string(b))
	}
	return nil
}

func (s *S3CompatibleProvider) getObject(key string) ([]byte, error) {
	objURL := fmt.Sprintf("%s/%s/%s", s.Endpoint, s.Bucket, key)
	req, err := http.NewRequest("GET", objURL, nil)
	if err != nil {
		return nil, err
	}
	s.signRequestV4(req, nil)
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("S3 GET %s devolvió %d", key, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (s *S3CompatibleProvider) signRequestV4(req *http.Request, payload []byte) {
	sum := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(sum[:])
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if s.AccessKey == "" || s.SecretKey == "" {
		return
	}

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", req.URL.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalReq := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canonicalHeaders, signedHeaders, payloadHash)

	credScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.Region)
	canonHash := sha256.Sum256([]byte(canonicalReq))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, credScope, hex.EncodeToString(canonHash[:]))

	kDate := hmacSHA256([]byte("AWS4"+s.SecretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(s.Region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.AccessKey, credScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// ============================================================================
// Driver 3: Folder / NAS Cloud Sync Provider
// ============================================================================

type FolderProvider struct {
	RootPath string
}

func NewFolderProvider(rootPath string) *FolderProvider {
	return &FolderProvider{RootPath: rootPath}
}

func (f *FolderProvider) Name() string {
	return fmt.Sprintf("Folder Sync (%s)", f.RootPath)
}

func (f *FolderProvider) UploadRelease(index *BundleIndex, chunks map[string][]byte) error {
	relDir := filepath.Join(f.RootPath, index.ReleaseKey)
	if err := os.MkdirAll(relDir, 0755); err != nil {
		return err
	}
	idxBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(relDir, "bundle_index.json"), idxBytes, 0600); err != nil {
		return err
	}
	for name, data := range chunks {
		if err := os.WriteFile(filepath.Join(relDir, name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (f *FolderProvider) DownloadRelease(releaseKey string) (*BundleIndex, map[string][]byte, error) {
	relDir := filepath.Join(f.RootPath, releaseKey)
	idxBytes, err := os.ReadFile(filepath.Join(relDir, "bundle_index.json"))
	if err != nil {
		return nil, nil, err
	}
	var index BundleIndex
	if err := json.Unmarshal(idxBytes, &index); err != nil {
		return nil, nil, err
	}
	chunks := make(map[string][]byte, len(index.Chunks))
	for _, meta := range index.Chunks {
		b, err := os.ReadFile(filepath.Join(relDir, meta.Name))
		if err != nil {
			return nil, nil, err
		}
		chunks[meta.Name] = b
	}
	return &index, chunks, nil
}

func (f *FolderProvider) ListReleases() ([]*BundleIndex, error) {
	entries, err := os.ReadDir(f.RootPath)
	if err != nil {
		return nil, err
	}
	var out []*BundleIndex
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.RootPath, e.Name(), "bundle_index.json"))
		if err == nil {
			var idx BundleIndex
			if json.Unmarshal(b, &idx) == nil {
				out = append(out, &idx)
			}
		}
	}
	return out, nil
}

func sanitizeReleaseKey(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, s)
}
