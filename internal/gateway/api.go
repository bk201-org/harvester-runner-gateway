package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource conflict")
	ErrInvalid  = errors.New("invalid resource")
)

const operationTimeout = 25 * time.Second

type VMRequest struct {
	Image         string   `json:"image"`
	Network       string   `json:"network"`
	CPU           int      `json:"cpu"`
	Memory        string   `json:"memory"`
	BootDiskSize  string   `json:"bootDiskSize"`
	SSHPublicKeys []string `json:"sshPublicKeys,omitempty"`
	UserData      string   `json:"userData,omitempty"`
	TTLSeconds    int      `json:"ttlSeconds,omitempty"`
}

type VolumeRequest struct {
	Size       string `json:"size"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
}

type VMStatus struct {
	ID                string    `json:"id"`
	Phase             string    `json:"phase"`
	PowerState        string    `json:"powerState"`
	IPAddresses       []string  `json:"ipAddresses"`
	AttachedVolumeIDs []string  `json:"attachedVolumeIDs"`
	Message           string    `json:"message,omitempty"`
	ExpiresAt         time.Time `json:"expiresAt"`
	RequestHash       string    `json:"-"`
}

type VolumeStatus struct {
	ID              string    `json:"id"`
	Phase           string    `json:"phase"`
	Size            string    `json:"size"`
	AttachedTo      string    `json:"attachedTo,omitempty"`
	AttachmentPhase string    `json:"attachmentPhase,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt"`
	RequestHash     string    `json:"-"`
}

type Backend interface {
	Ping(context.Context) error
	CountVMs(context.Context, config.RepositoryPolicy) (int, error)
	ListVMs(context.Context, config.RepositoryPolicy, auth.Owner) ([]VMStatus, error)
	GetVM(context.Context, config.RepositoryPolicy, auth.Owner, string) (VMStatus, error)
	CreateVM(context.Context, config.RepositoryPolicy, auth.Owner, string, VMRequest, time.Time, string) (VMStatus, error)
	DeleteVM(context.Context, config.RepositoryPolicy, auth.Owner, string) error
	PowerVM(context.Context, config.RepositoryPolicy, auth.Owner, string, string) error
	RebootVM(context.Context, config.RepositoryPolicy, auth.Owner, string) error
	CountVolumes(context.Context, config.RepositoryPolicy) (int, error)
	ListVolumes(context.Context, config.RepositoryPolicy, auth.Owner) ([]VolumeStatus, error)
	GetVolume(context.Context, config.RepositoryPolicy, auth.Owner, string) (VolumeStatus, error)
	CreateVolume(context.Context, config.RepositoryPolicy, auth.Owner, string, VolumeRequest, time.Time, string) (VolumeStatus, error)
	DeleteVolume(context.Context, config.RepositoryPolicy, auth.Owner, string) error
	AttachVolume(context.Context, config.RepositoryPolicy, auth.Owner, string, string) error
	DetachVolume(context.Context, config.RepositoryPolicy, auth.Owner, string, string) error
	CleanupExpired(context.Context, time.Time) error
}

type TokenVerifier interface {
	Verify(context.Context, string, config.Config) (auth.Owner, config.RepositoryPolicy, error)
}

type Server struct {
	cfg      config.Config
	verifier TokenVerifier
	backend  Backend
	opGate   chan struct{}
	Handler  http.Handler
}

func NewServer(cfg config.Config, verifier TokenVerifier, backend Backend) *Server {
	s := &Server{cfg: cfg, verifier: verifier, backend: backend, opGate: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := backend.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "cluster_unavailable", "Harvester API is unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/quota", s.authorize(s.quota))
	mux.HandleFunc("GET /v1/vms", s.authorize(s.listVMs))
	mux.HandleFunc("POST /v1/vms", s.authorize(s.createVM))
	mux.HandleFunc("GET /v1/vms/{id}", s.authorize(s.getVM))
	mux.HandleFunc("DELETE /v1/vms/{id}", s.authorize(s.deleteVM))
	mux.HandleFunc("PUT /v1/vms/{id}/power", s.authorize(s.powerVM))
	mux.HandleFunc("POST /v1/vms/{id}/reboot", s.authorize(s.rebootVM))
	mux.HandleFunc("PUT /v1/vms/{id}/volumes/{volumeID}", s.authorize(s.attachVolume))
	mux.HandleFunc("DELETE /v1/vms/{id}/volumes/{volumeID}", s.authorize(s.detachVolume))
	mux.HandleFunc("GET /v1/volumes", s.authorize(s.listVolumes))
	mux.HandleFunc("POST /v1/volumes", s.authorize(s.createVolume))
	mux.HandleFunc("GET /v1/volumes/{id}", s.authorize(s.getVolume))
	mux.HandleFunc("DELETE /v1/volumes/{id}", s.authorize(s.deleteVolume))
	s.Handler = mux
	return s
}

func (s *Server) CleanupExpired(ctx context.Context, now time.Time) error {
	ctx, finish, err := s.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return s.backend.CleanupExpired(ctx, now)
}

func (s *Server) beginOperation(parent context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(parent, operationTimeout)
	select {
	case s.opGate <- struct{}{}:
		return ctx, func() {
			<-s.opGate
			cancel()
		}, nil
	case <-ctx.Done():
		cancel()
		return nil, nil, ctx.Err()
	}
}

func (s *Server) beginRequestOperation(w http.ResponseWriter, r *http.Request) (*http.Request, func(), bool) {
	ctx, finish, err := s.beginOperation(r.Context())
	if err != nil {
		backendError(w, err)
		return nil, nil, false
	}
	return r.WithContext(ctx), finish, true
}

type action func(http.ResponseWriter, *http.Request, auth.Owner, config.RepositoryPolicy)

func (s *Server) authorize(next action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Bearer token required")
			return
		}
		owner, policy, err := s.verifier.Verify(r.Context(), strings.TrimPrefix(header, "Bearer "), s.cfg)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Bearer token rejected")
			return
		}
		next(w, r, owner, policy)
	}
}

func (s *Server) quota(w http.ResponseWriter, r *http.Request, _ auth.Owner, policy config.RepositoryPolicy) {
	vms, err := s.backend.CountVMs(r.Context(), policy)
	if err != nil {
		backendError(w, err)
		return
	}
	volumes, err := s.backend.CountVolumes(r.Context(), policy)
	if err != nil {
		backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"maxActiveVMs": policy.Quota.MaxActiveVMs, "activeVMs": vms,
		"maxActiveVolumes": policy.Quota.MaxActiveVolumes, "activeVolumes": volumes,
	})
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	items, err := s.backend.ListVMs(r.Context(), policy, owner)
	if err != nil {
		backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	item, err := s.backend.GetVM(r.Context(), policy, owner, r.PathValue("id"))
	if err != nil {
		backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	var req VMRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateVMRequest(&req, policy); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	id, digest := resourceIdentity(owner, "vm", key, req)
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	for _, candidate := range []string{id, legacyResourceID(owner, "vm", key)} {
		if existing, err := s.backend.GetVM(r.Context(), policy, owner, candidate); err == nil {
			if existing.RequestHash != digest {
				writeError(w, http.StatusConflict, "idempotency_conflict", "key was used with a different request")
				return
			}
			writeJSON(w, http.StatusOK, existing)
			return
		} else if !errors.Is(err, ErrNotFound) {
			backendError(w, err)
			return
		}
	}
	count, err := s.backend.CountVMs(r.Context(), policy)
	if err != nil {
		backendError(w, err)
		return
	}
	if count >= policy.Quota.MaxActiveVMs {
		writeError(w, http.StatusConflict, "quota_exceeded", "active VM quota reached")
		return
	}
	item, err := s.backend.CreateVM(r.Context(), policy, owner, id, req, time.Now().Add(time.Duration(req.TTLSeconds)*time.Second), digest)
	if err != nil {
		backendError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/vms/"+id)
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	if err := s.backend.DeleteVM(r.Context(), policy, owner, r.PathValue("id")); err != nil {
		backendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) powerVM(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	var req struct {
		State string `json:"state"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.State != "on" && req.State != "off" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "state must be on or off")
		return
	}
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	if err := s.backend.PowerVM(r.Context(), policy, owner, r.PathValue("id"), req.State); err != nil {
		backendError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) rebootVM(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	if err := s.backend.RebootVM(r.Context(), policy, owner, r.PathValue("id")); err != nil {
		backendError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	items, err := s.backend.ListVolumes(r.Context(), policy, owner)
	if err != nil {
		backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getVolume(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	item, err := s.backend.GetVolume(r.Context(), policy, owner, r.PathValue("id"))
	if err != nil {
		backendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	var req VolumeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateVolumeRequest(&req, policy); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	id, digest := resourceIdentity(owner, "volume", key, req)
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	for _, candidate := range []string{id, legacyResourceID(owner, "volume", key)} {
		if existing, err := s.backend.GetVolume(r.Context(), policy, owner, candidate); err == nil {
			if existing.RequestHash != digest {
				writeError(w, http.StatusConflict, "idempotency_conflict", "key was used with a different request")
				return
			}
			writeJSON(w, http.StatusOK, existing)
			return
		} else if !errors.Is(err, ErrNotFound) {
			backendError(w, err)
			return
		}
	}
	count, err := s.backend.CountVolumes(r.Context(), policy)
	if err != nil {
		backendError(w, err)
		return
	}
	if count >= policy.Quota.MaxActiveVolumes {
		writeError(w, http.StatusConflict, "quota_exceeded", "active volume quota reached")
		return
	}
	item, err := s.backend.CreateVolume(r.Context(), policy, owner, id, req, time.Now().Add(time.Duration(req.TTLSeconds)*time.Second), digest)
	if err != nil {
		backendError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/volumes/"+id)
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	if err := s.backend.DeleteVolume(r.Context(), policy, owner, r.PathValue("id")); err != nil {
		backendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) attachVolume(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	if err := s.backend.AttachVolume(r.Context(), policy, owner, r.PathValue("id"), r.PathValue("volumeID")); err != nil {
		backendError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) detachVolume(w http.ResponseWriter, r *http.Request, owner auth.Owner, policy config.RepositoryPolicy) {
	r, finish, ok := s.beginRequestOperation(w, r)
	if !ok {
		return
	}
	defer finish()
	if err := s.backend.DetachVolume(r.Context(), policy, owner, r.PathValue("id"), r.PathValue("volumeID")); err != nil {
		backendError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func validateVMRequest(req *VMRequest, p config.RepositoryPolicy) error {
	if !contains(p.Images, req.Image) || !contains(p.Networks, req.Network) {
		return fmt.Errorf("image or network is not allowed")
	}
	if req.CPU < 1 || req.CPU > p.MaxCPU || !validQuantity(req.Memory, p.MaxMemory) || !validQuantity(req.BootDiskSize, p.MaxBootDiskSize) {
		return fmt.Errorf("CPU, memory, or boot disk size exceeds configured limits")
	}
	if len(req.UserData) > 64*1024 || (req.UserData != "" && !strings.HasPrefix(req.UserData, "#cloud-config")) {
		return fmt.Errorf("userData must be cloud-config of at most 64 KiB")
	}
	if len(req.SSHPublicKeys) > 10 {
		return fmt.Errorf("at most 10 SSH public keys are allowed")
	}
	for _, key := range req.SSHPublicKeys {
		fields := strings.Fields(key)
		if len(fields) < 2 || len(key) > 8192 || (fields[0] != "ssh-ed25519" && fields[0] != "ssh-rsa" && !strings.HasPrefix(fields[0], "ecdsa-sha2-")) {
			return fmt.Errorf("invalid SSH public key")
		}
		if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
			return fmt.Errorf("invalid SSH public key encoding")
		}
	}
	return normalizeTTL(&req.TTLSeconds)
}

func validateVolumeRequest(req *VolumeRequest, p config.RepositoryPolicy) error {
	if !validQuantity(req.Size, p.MaxVolumeSize) {
		return fmt.Errorf("volume size exceeds configured limit")
	}
	return normalizeTTL(&req.TTLSeconds)
}

func normalizeTTL(seconds *int) error {
	if *seconds == 0 {
		*seconds = int(config.DefaultTTL.Seconds())
	}
	if *seconds < 1 || *seconds > int(config.MaximumTTL.Seconds()) {
		return fmt.Errorf("ttlSeconds must be between 1 and %d", int(config.MaximumTTL.Seconds()))
	}
	return nil
}

func validQuantity(value, maximum string) bool {
	quantity, err := resource.ParseQuantity(value)
	if err != nil || quantity.Sign() <= 0 {
		return false
	}
	max, err := resource.ParseQuantity(maximum)
	return err == nil && quantity.Cmp(max) <= 0
}

func resourceIdentity(owner auth.Owner, kind, key string, request any) (string, string) {
	data, _ := json.Marshal(request)
	digest := sha256.Sum256(data)
	return resourceID(owner, kind, key, "hrgw-", 10), hex.EncodeToString(digest[:])
}

func legacyResourceID(owner auth.Owner, kind, key string) string {
	return resourceID(owner, kind, key, "rgw-", 15)
}

func resourceID(owner auth.Owner, kind, key, prefix string, hashBytes int) string {
	identity := sha256.Sum256([]byte(owner.RepositoryID + "/" + owner.RunID + "/" + owner.RunAttempt + "/" + kind + "/" + key))
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(identity[:hashBytes]))
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || !printableASCII(key) {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 1-128 printable characters")
		return "", false
	}
	return key, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "only one JSON object is allowed")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func backendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, http.StatusGatewayTimeout, "cluster_timeout", "Harvester operation timed out")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, "invalid_resource", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "cluster_error", "Harvester operation failed")
	}
}

func contains(values []string, value string) bool {
	for _, allowed := range values {
		if allowed == value {
			return true
		}
	}
	return false
}

func printableASCII(value string) bool {
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}
