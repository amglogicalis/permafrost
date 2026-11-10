package cloud

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amglogicalis/permafrost/internal/engine"
	"github.com/amglogicalis/permafrost/internal/vault"
)

func TestGitHubReleasesChunkedZeroKnowledgeSync(t *testing.T) {
	// 1. Setup Mock GitHub Releases API + Upload Server
	var mu sync.Mutex
	assetsStore := make(map[string][]byte)
	var currentTag string

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/amglogicalis/permafrost-vault/releases":
			var reqBody map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			currentTag, _ = reqBody["tag_name"].(string)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(ghReleaseResponse{
				ID:      101,
				TagName: currentTag,
			})

		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/repos/amglogicalis/permafrost-vault/releases/101/assets"):
			name := r.URL.Query().Get("name")
			data, _ := io.ReadAll(r.Body)
			assetsStore[name] = data
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(ghAssetEntry{
				ID:   int64(len(assetsStore)),
				Name: name,
				Size: int64(len(data)),
				URL:  srv.URL + "/download/" + name,
			})

		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/amglogicalis/permafrost-vault/releases/tags/"):
			var assets []ghAssetEntry
			idx := int64(1)
			for name, data := range assetsStore {
				assets = append(assets, ghAssetEntry{
					ID:   idx,
					Name: name,
					Size: int64(len(data)),
					URL:  srv.URL + "/download/" + name,
				})
				idx++
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(ghReleaseResponse{
				ID:      101,
				TagName: currentTag,
				Assets:  assets,
			})

		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/download/"):
			name := strings.TrimPrefix(r.URL.Path, "/download/")
			data, ok := assetsStore[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// 2. Create source Vault & freeze a project
	srcVaultDir := t.TempDir()
	srcProjDir := t.TempDir()

	vSrc, err := vault.Open(srcVaultDir)
	if err != nil {
		t.Fatal(err)
	}
	engSrc, err := engine.New(vSrc)
	if err != nil {
		t.Fatal(err)
	}

	secretContent := "TERRAFORM_STATE_SECRET=super_confidential_value_1234567890\n" + strings.Repeat("DATA_LINE\n", 200)
	if err := os.WriteFile(filepath.Join(srcProjDir, "terraform.tfstate"), []byte(secretContent), 0644); err != nil {
		t.Fatal(err)
	}

	m1, err := engSrc.Freeze(srcProjDir, engine.FreezeOptions{Tag: "v1.0-cloud", Message: "Cloud encrypted test"})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Pack with forced small chunk size (256 bytes) to verify multi-chunk splitting + Zero-Knowledge AES-256-GCM
	cmSrc := New(vSrc)
	passphrase := "pfk_test_master_secret_key"
	idx, chunks, err := cmSrc.PackToChunks(m1, passphrase, 256)
	if err != nil {
		t.Fatalf("PackToChunks failed: %v", err)
	}
	if !idx.Encrypted {
		t.Fatalf("expected bundle to be encrypted when ZeroKnowledgeEncryption is ON")
	}
	if len(chunks) < 2 {
		t.Fatalf("expected payload to be split into multiple chunks (<256B each), got %d chunk(s)", len(chunks))
	}

	ghProvider := NewGitHubReleasesProvider("amglogicalis/permafrost-vault", "mock-token")
	ghProvider.APIBaseURL = srv.URL
	ghProvider.UploadBaseURL = srv.URL

	if err := ghProvider.UploadRelease(idx, chunks); err != nil {
		t.Fatalf("GitHub UploadRelease failed: %v", err)
	}

	// 4. Download into a completely separate destination Vault & restore to a new folder
	dstVaultDir := t.TempDir()
	dstProjDir := t.TempDir()

	vDst, err := vault.Open(dstVaultDir)
	if err != nil {
		t.Fatal(err)
	}
	engDst, err := engine.New(vDst)
	if err != nil {
		t.Fatal(err)
	}
	cmDst := New(vDst)

	dlIdx, dlChunks, err := ghProvider.DownloadRelease(idx.ReleaseKey)
	if err != nil {
		t.Fatalf("GitHub DownloadRelease failed: %v", err)
	}

	// Verify wrong passphrase fails decryption
	if _, err := cmDst.UnpackFromChunks(dlIdx, dlChunks, "wrong-password", dstProjDir); err == nil {
		t.Fatalf("expected UnpackFromChunks to reject wrong passphrase")
	}

	// Unpack with correct passphrase and restore
	restoredManifest, err := cmDst.UnpackFromChunks(dlIdx, dlChunks, passphrase, dstProjDir)
	if err != nil {
		t.Fatalf("UnpackFromChunks failed: %v", err)
	}
	if _, err := engDst.Restore(dstProjDir, restoredManifest, nil, true); err != nil {
		t.Fatalf("Restore from cloud failed: %v", err)
	}

	gotBytes, err := os.ReadFile(filepath.Join(dstProjDir, "terraform.tfstate"))
	if err != nil || string(gotBytes) != secretContent {
		t.Fatalf("restored file content mismatch after GitHub Releases multi-chunk sync")
	}
}

func TestS3ProviderAndTeleportLifecycle(t *testing.T) {
	// 1. Mock S3 server
	var mu sync.Mutex
	s3Store := make(map[string][]byte)
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			b, _ := io.ReadAll(r.Body)
			s3Store[r.URL.Path] = b
			w.WriteHeader(http.StatusOK)
		case "GET":
			b, ok := s3Store[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
		}
	}))
	defer s3Srv.Close()

	vaultDir := t.TempDir()
	projDir := t.TempDir()
	teleportDestDir := t.TempDir()

	v, err := vault.Open(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(v)
	if err != nil {
		t.Fatal(err)
	}
	cm := New(v)

	if err := os.WriteFile(filepath.Join(projDir, "app.env"), []byte("PORT=8080\nSECRET=xyz"), 0644); err != nil {
		t.Fatal(err)
	}
	m1, err := eng.Freeze(projDir, engine.FreezeOptions{Tag: "v1-teleport", Message: "Teleport test"})
	if err != nil {
		t.Fatal(err)
	}

	// Test S3 Upload & Download with SigV4
	s3Prov := NewS3CompatibleProvider(s3Srv.URL, "pfrost-bucket", "eu-west-1")
	s3Prov.AccessKey = "AKIAIOSFODNN7EXAMPLE"
	s3Prov.SecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"

	idx, chunks, err := cm.PackToChunks(m1, "s3-secret-key", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := s3Prov.UploadRelease(idx, chunks); err != nil {
		t.Fatalf("S3 UploadRelease failed: %v", err)
	}
	dlIdx, dlChunks, err := s3Prov.DownloadRelease(idx.ReleaseKey)
	if err != nil || len(dlChunks) != len(chunks) || dlIdx.ReleaseKey != idx.ReleaseKey {
		t.Fatalf("S3 DownloadRelease failed: %v", err)
	}

	// Test Teleport Create, Receive, Revoke, and Toggle OFF
	cap, err := cm.CreateTeleport(m1, "teleport-pass", 1*time.Hour)
	if err != nil {
		t.Fatalf("CreateTeleport failed: %v", err)
	}
	if !cap.Encrypted || !strings.HasPrefix(cap.Code, "tp_") {
		t.Fatalf("unexpected teleport capsule: %+v", cap)
	}

	_, _, err = cm.ReceiveTeleport(eng, cap.Code, teleportDestDir, "teleport-pass")
	if err != nil {
		t.Fatalf("ReceiveTeleport failed: %v", err)
	}
	gotEnv, err := os.ReadFile(filepath.Join(teleportDestDir, "app.env"))
	if err != nil || string(gotEnv) != "PORT=8080\nSECRET=xyz" {
		t.Fatalf("teleported file content mismatch: %q", string(gotEnv))
	}

	// Revoke Teleport
	n, err := cm.RevokeTeleport(cap.Code)
	if err != nil || n != 1 {
		t.Fatalf("RevokeTeleport failed: %v", err)
	}

	// Toggle Teleport OFF -> must reject CreateTeleport transparently
	_ = v.Config.SetFeature("teleport", false)
	if _, err := cm.CreateTeleport(m1, "teleport-pass", 1*time.Hour); err == nil {
		t.Fatalf("expected CreateTeleport to fail when Teleport module is OFF")
	}
}
