package confighome

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	labelRe      = mustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	envNameRe    = mustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretKeyRe  = mustCompile(`^[A-Za-z0-9._-]+$`)
	repoRefRe    = mustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	whitespaceRe = mustCompile(`\s`)

	interfaceNameRe = mustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	// Trailing slash rejected to match scripts/interface_validate.mjs
	// (byte-exact mount paths, no trailing-slash normalization).
	interfacePathRe = mustCompile(`^/.+[^/]$`)
	interfaceEnvRe  = mustCompile(`^[A-Z][A-Z0-9_]*$`)
	interfaceVarRe  = mustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	interfaceSlotRe = mustCompile(`^[a-z][a-zA-Z0-9_]*$`)
)

func mustCompile(pattern string) *regexp.Regexp {
	re, err := regexp.Compile(pattern)
	if err != nil {
		panic("invalid pattern: " + err.Error())
	}
	return re
}

// Validate enforces the record's fail-closed shape constraints — the
// record-level half of the criteria-k8s gates (the assembled payload's
// half lives in Assembly.Assemble). Castle validates SHAPE only: field
// names, closed vocabularies, and cross-references; values are opaque.
func (r *Record) Validate() error {
	if r.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, r.APIVersion)
	}
	if r.Kind != KindRecord {
		return fmt.Errorf("kind must be %q, got %q", KindRecord, r.Kind)
	}
	if strings.TrimSpace(r.RegistryName) == "" {
		return fmt.Errorf("registryName required: the record is the project's registry entry")
	}
	if !labelRe.MatchString(r.Namespace) {
		return fmt.Errorf("namespace %q is not a DNS-1123 label", r.Namespace)
	}
	for _, repo := range r.Repos {
		if !repoRefRe.MatchString(repo) {
			return fmt.Errorf("repo %q is not an %q reference", repo, "owner/name")
		}
	}
	switch r.Cutover {
	case "", CutoverDone:
	default:
		return fmt.Errorf("cutover must be %q or absent, got %q (binding-first, deletion second)", CutoverDone, r.Cutover)
	}
	switch r.ImageConflictPolicy {
	case "", ImageConflictPolicyFailClosed, ImageConflictPolicyPreferEvent, ImageConflictPolicyPreferMetadata:
	default:
		return fmt.Errorf("imageConflictPolicy must be %q, %q or %q, got %q", ImageConflictPolicyFailClosed, ImageConflictPolicyPreferEvent, ImageConflictPolicyPreferMetadata, r.ImageConflictPolicy)
	}
	for name, wf := range r.Catalog {
		if err := validateWorkflowShape(name, wf); err != nil {
			return fmt.Errorf("catalog[%q]: %w", name, err)
		}
	}
	for name, entry := range r.ConfigLibrary {
		if err := validateConfigEntry(name, entry); err != nil {
			return fmt.Errorf("configLibrary[%q]: %w", name, err)
		}
	}
	seenRoutes := make(map[string]bool, len(r.Routes))
	for i, rt := range r.Routes {
		if !labelRe.MatchString(rt.Name) {
			return fmt.Errorf("routes[%d].name %q is not a DNS-1123 label", i, rt.Name)
		}
		if seenRoutes[rt.Name] {
			return fmt.Errorf("routes[%d].name %q is not unique", i, rt.Name)
		}
		seenRoutes[rt.Name] = true
		if rt.Project != r.RegistryName {
			return fmt.Errorf("routes[%d] (%s): project %q must be the record's registryName %q (a record carries only its own routes)", i, rt.Name, rt.Project, r.RegistryName)
		}
		if _, ok := r.Catalog[rt.Workflow]; !ok {
			return fmt.Errorf("routes[%d] (%s): workflow %q is not present in the record's catalog", i, rt.Name, rt.Workflow)
		}
		if rt.ConfigRef != "" {
			if _, ok := r.ConfigLibrary[rt.ConfigRef]; !ok {
				return fmt.Errorf("routes[%d] (%s): configRef %q is not present in the record's configLibrary", i, rt.Name, rt.ConfigRef)
			}
		}
		if len(rt.States) == 0 {
			return fmt.Errorf("routes[%d] (%s): states must not be empty (records store states materialized: the payload default is [%s])", i, rt.Name, DefaultState)
		}
		for _, s := range rt.States {
			if s == "" {
				return fmt.Errorf("routes[%d] (%s): states must not contain empty names", i, rt.Name)
			}
		}
		for _, tag := range rt.Tags {
			if tag == "" {
				return fmt.Errorf("routes[%d] (%s): tags must not contain empty names", i, rt.Name)
			}
		}
		switch rt.TagMatch {
		case "", TagMatchAll, TagMatchAny:
		default:
			return fmt.Errorf("routes[%d] (%s): tagMatch must be %q or %q, got %q", i, rt.Name, TagMatchAll, TagMatchAny, rt.TagMatch)
		}
	}
	if r.Binding != nil {
		if err := ValidateBinding(r.Binding); err != nil {
			return fmt.Errorf("binding: %w", err)
		}
	}
	for name, digest := range r.Digests {
		if _, ok := r.Catalog[name]; !ok {
			return fmt.Errorf("digests[%q]: no catalog entry named %q", name, name)
		}
		if !isHex64(digest) {
			return fmt.Errorf("digests[%q]: %q is not a sha256 hex digest", name, digest)
		}
	}
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validateWorkflowShape enforces the per-workflow constraints including
// the ADR-0005 D1/D2 source modes: type=image means image-only (no
// url/ref, no interface), type=url requires the url and a declared
// interface (KB-310).
func validateWorkflowShape(name string, wf Workflow) error {
	if !labelRe.MatchString(name) {
		return fmt.Errorf("name is not a DNS-1123 label")
	}
	switch wf.Type {
	case TypeImage:
		if wf.Image == "" {
			return fmt.Errorf("type=image requires an image")
		}
		if wf.URL != "" || wf.Ref != "" {
			return fmt.Errorf("type=image must not declare url or ref")
		}
		if wf.Interface != nil {
			return fmt.Errorf("type=image must not declare interface")
		}
	case TypeURL:
		if wf.URL == "" {
			return fmt.Errorf("type=url requires a url")
		}
		if wf.Interface == nil {
			return fmt.Errorf("type=url requires interface (KB-310)")
		}
		if err := validateInterface(wf.Interface); err != nil {
			return fmt.Errorf("interface: %w", err)
		}
	default:
		return fmt.Errorf("type must be %q or %q, got %q", TypeImage, TypeURL, wf.Type)
	}
	switch wf.Class {
	case "", ClassDev, ClassTriage:
	default:
		return fmt.Errorf("class must be %q or %q, got %q", ClassDev, ClassTriage, wf.Class)
	}
	switch wf.ImageConflictPolicy {
	case "", ImageConflictPolicyFailClosed, ImageConflictPolicyPreferEvent, ImageConflictPolicyPreferMetadata:
	default:
		return fmt.Errorf("imageConflictPolicy must be %q, %q or %q, got %q", ImageConflictPolicyFailClosed, ImageConflictPolicyPreferEvent, ImageConflictPolicyPreferMetadata, wf.ImageConflictPolicy)
	}
	if wf.Namespace != "" && !labelRe.MatchString(wf.Namespace) {
		return fmt.Errorf("namespace %q is not a DNS-1123 label", wf.Namespace)
	}
	if err := validateEnvMap(wf.Env, "env"); err != nil {
		return err
	}
	if err := validateAdapterImages(wf.AdapterImages); err != nil {
		return err
	}
	var seenV []string
	for i, v := range wf.Volumes {
		if err := validateVolume(v); err != nil {
			return fmt.Errorf("volumes[%d] (%s): %w", i, v.Name, err)
		}
		if slices.Contains(seenV, v.Name) {
			return fmt.Errorf("volumes[%d] (%s): name is not unique within the workflow", i, v.Name)
		}
		seenV = append(seenV, v.Name)
	}
	var seenS []string
	for i, s := range wf.Secrets {
		if err := validateSecret(s); err != nil {
			return fmt.Errorf("secrets[%d] (%s): %w", i, s.Name, err)
		}
		if slices.Contains(seenS, s.Name) {
			return fmt.Errorf("secrets[%d] (%s): name is not unique within the workflow", i, s.Name)
		}
		seenS = append(seenS, s.Name)
	}
	return nil
}

// validateInterface enforces the shape constraints of the tree
// interface declaration (KB-310): declared apiVersion/kind, the tree
// name, mount shapes (pvc|tmp at an absolute non-recursive path), env
// lanes, and config slots restricted to the platform key surface.
func validateInterface(iface *Interface) error {
	if iface.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, iface.APIVersion)
	}
	if iface.Kind != KindInterface {
		return fmt.Errorf("kind must be %q, got %q", KindInterface, iface.Kind)
	}
	if iface.Workflow == "" || !interfaceNameRe.MatchString(iface.Workflow) {
		return fmt.Errorf("workflow (%q) is not a tree name", iface.Workflow)
	}
	for _, kind := range iface.Adapters {
		if !interfaceNameRe.MatchString(kind) {
			return fmt.Errorf("adapter kind %q is not a valid kind name", kind)
		}
	}
	if hasDuplicate(iface.Adapters) {
		return fmt.Errorf("adapter kinds are not unique")
	}
	seenMounts := make(map[string]bool, len(iface.Mounts))
	for i, m := range iface.Mounts {
		if !interfaceNameRe.MatchString(m.Name) {
			return fmt.Errorf("mounts[%d]: name %q is not a mount name", i, m.Name)
		}
		if seenMounts[m.Name] {
			return fmt.Errorf("mounts[%d] (%s): name is not unique", i, m.Name)
		}
		seenMounts[m.Name] = true
		if !interfacePathRe.MatchString(m.MountPath) {
			return fmt.Errorf("mounts[%d] (%s): mountPath %q must be an absolute path without trailing slash", i, m.Name, m.MountPath)
		}
		if m.Kind != VolumePVC && m.Kind != VolumeTmp {
			return fmt.Errorf("mounts[%d] (%s): kind %q must be pvc or tmp", i, m.Name, m.Kind)
		}
	}
	if hasDuplicate(iface.SecretEnv) {
		return fmt.Errorf("secretEnv lanes are not unique")
	}
	for _, name := range iface.SecretEnv {
		if !interfaceEnvRe.MatchString(name) {
			return fmt.Errorf("secretEnv %q is not an env name (UPPER_SNAKE)", name)
		}
	}
	if hasDuplicate(iface.Env) {
		return fmt.Errorf("env names are not unique")
	}
	for _, name := range iface.Env {
		if !interfaceEnvRe.MatchString(name) {
			return fmt.Errorf("env %q is not an env name (UPPER_SNAKE)", name)
		}
	}
	seenSlots := make(map[string]bool, len(iface.ConfigSlots))
	for i, s := range iface.ConfigSlots {
		if !interfaceSlotRe.MatchString(s.Slot) {
			return fmt.Errorf("configSlots[%d]: slot %q is not a slot name (lowercase-first)", i, s.Slot)
		}
		if seenSlots[s.Slot] {
			return fmt.Errorf("configSlots[%d] (%s): slot is not unique", i, s.Slot)
		}
		seenSlots[s.Slot] = true
		wantVar, ok := KB310ConfigSlotVariables[s.Slot]
		if !ok {
			return fmt.Errorf("configSlots[%d] (%s): slot is not a platform config key (declare only buildCmd, testCmd, ciGateCmd, or baseBranch — the keys a project binding can carry, KB-310)", i, s.Slot)
		}
		if s.Variable != wantVar {
			return fmt.Errorf("configSlots[%d] (%s): variable %q is not the slot's delivery variable %q (the KB-103 render delivers the value as the tree variable, KB-310)", i, s.Slot, s.Variable, wantVar)
		}
		if !interfaceVarRe.MatchString(s.Variable) {
			return fmt.Errorf("configSlots[%d] (%s): variable %q is not a tree variable", i, s.Slot, s.Variable)
		}
		if s.Target == "" {
			return fmt.Errorf("configSlots[%d] (%s): target is required", i, s.Slot)
		}
	}
	switch iface.LockKey {
	case "", InterfaceLockKeyRepoURL:
	default:
		return fmt.Errorf("lockKey must be %q or absent, got %q", InterfaceLockKeyRepoURL, iface.LockKey)
	}
	return nil
}

func hasDuplicate(names []string) bool {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			return true
		}
		seen[name] = true
	}
	return false
}

// validateConfigEntry enforces the per-config constraints (KB-103,
// KB-154): DNS-1123 name (it doubles as the pinned ConfigMap's name),
// non-blank declared commands, whitespace-free baseBranch, and the
// tag vocabularies.
func validateConfigEntry(name string, entry ConfigEntry) error {
	if !labelRe.MatchString(name) {
		return fmt.Errorf("name is not a DNS-1123 label")
	}
	for field, value := range map[string]string{
		"buildCmd":   entry.BuildCmd,
		"testCmd":    entry.TestCmd,
		"ciGateCmd":  entry.CIGateCmd,
		"baseBranch": entry.BaseBranch,
	} {
		if value == "" {
			continue
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be blank when declared", field)
		}
	}
	if whitespaceRe.MatchString(entry.BaseBranch) {
		return fmt.Errorf("baseBranch %q must be a git ref name without whitespace", entry.BaseBranch)
	}
	for _, tag := range entry.Tags {
		if tag == "" {
			return fmt.Errorf("tags must not contain empty names")
		}
	}
	switch entry.TagMatch {
	case "", TagMatchAll, TagMatchAny:
	default:
		return fmt.Errorf("tagMatch must be %q or %q, got %q", TagMatchAll, TagMatchAny, entry.TagMatch)
	}
	return nil
}

// validateVolume enforces the pvc/nfs/tmp/k8s-secret shapes: sibling-kind
// fields must stay unset (fail-closed exclusivity).
func validateVolume(v Volume) error {
	if !labelRe.MatchString(v.Name) {
		return fmt.Errorf("name %q is not a DNS-1123 label", v.Name)
	}
	if !strings.HasPrefix(v.MountPath, "/") {
		return fmt.Errorf("mountPath %q must be absolute", v.MountPath)
	}
	if err := validateEnvMap(v.Env, "env"); err != nil {
		return err
	}
	switch v.Kind {
	case VolumePVC:
		if v.Claim == "" {
			return fmt.Errorf("kind=pvc requires a claim")
		}
		if v.Server != "" || v.Path != "" || v.SizeLimit != "" || v.SecretName != "" {
			return fmt.Errorf("kind=pvc must not declare server, path, sizeLimit, or secretName")
		}
	case VolumeNFS:
		if v.Server == "" || v.Path == "" {
			return fmt.Errorf("kind=nfs requires server and path")
		}
		if v.Claim != "" || v.SizeLimit != "" || v.SecretName != "" {
			return fmt.Errorf("kind=nfs must not declare claim, sizeLimit, or secretName")
		}
	case VolumeTmp:
		if v.Claim != "" || v.Server != "" || v.Path != "" || v.SecretName != "" {
			return fmt.Errorf("kind=tmp must not declare claim, server, path, or secretName")
		}
	case VolumeK8sSecret:
		if v.SecretName == "" {
			return fmt.Errorf("kind=k8s-secret requires a secretName")
		}
		if v.Claim != "" || v.Server != "" || v.Path != "" || v.SizeLimit != "" {
			return fmt.Errorf("kind=k8s-secret must not declare claim, server, path, or sizeLimit")
		}
	default:
		return fmt.Errorf("kind must be %q, %q, %q, or %q, got %q", VolumePVC, VolumeNFS, VolumeTmp, VolumeK8sSecret, v.Kind)
	}
	return nil
}

// validateSecret enforces name-only secret references: no credential
// material lives in config (CRI-214 section 3.8).
func validateSecret(s Secret) error {
	if !labelRe.MatchString(s.Name) {
		return fmt.Errorf("name %q is not a DNS-1123 label", s.Name)
	}
	if s.SecretProviderClass == "" {
		return fmt.Errorf("secretProviderClass is required")
	}
	if !strings.HasPrefix(s.MountPath, "/") {
		return fmt.Errorf("mountPath %q must be absolute", s.MountPath)
	}
	return validateSecretEnv(s.Env)
}

// validateEnvMap checks env var names and non-empty string values.
func validateEnvMap(env map[string]string, where string) error {
	for name, value := range env {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("%s has invalid env var name %q", where, name)
		}
		if value == "" {
			return fmt.Errorf("%s.%s must be a string value", where, name)
		}
	}
	return nil
}

// validateSecretEnv checks the secret's env mapping: env var names to
// rendered-file key names — names only, never values.
func validateSecretEnv(env map[string]string) error {
	for name, key := range env {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("env has invalid env var name %q", name)
		}
		if !secretKeyRe.MatchString(key) {
			return fmt.Errorf("env.%s value %q is not a valid key name", name, key)
		}
	}
	return nil
}

// validateAdapterImages checks per-kind adapter image references: keys
// are adapter kinds (DNS-1123 labels), values non-empty without
// whitespace.
func validateAdapterImages(images map[string]string) error {
	for kind, ref := range images {
		if !labelRe.MatchString(kind) {
			return fmt.Errorf("adapterImages has invalid adapter kind %q", kind)
		}
		if ref == "" || whitespaceRe.MatchString(ref) {
			return fmt.Errorf("adapterImages.%s must be a non-empty image reference without whitespace", kind)
		}
	}
	return nil
}

// BindingRules carries a binding payload's raw parts through the same
// validators the routes package exposes to its binding package.
type BindingRules struct {
	Images  map[string]string
	Env     map[string]string
	Volumes []Volume
	Secrets []Secret
	Config  map[string]string
}

// Validate enforces the binding semantic rules (KB-311, ADR 307): the
// fixed apiVersion/kind are checked by callers; here adapter images,
// env, per-kind volume/secret shapes (unique names), and the KB-311
// config slot surface validate fail-closed.
func (b BindingRules) Validate() error {
	if err := validateAdapterImages(b.Images); err != nil {
		return fmt.Errorf("binding images: %w", err)
	}
	if err := validateEnvMap(b.Env, "env"); err != nil {
		return fmt.Errorf("binding %w", err)
	}
	seen := map[string]bool{}
	for i, v := range b.Volumes {
		if seen[v.Name] {
			return fmt.Errorf("binding volumes[%d] duplicates name %q", i, v.Name)
		}
		seen[v.Name] = true
		if err := validateVolume(v); err != nil {
			return fmt.Errorf("binding volume %q: %w", v.Name, err)
		}
	}
	clear(seen)
	for i, s := range b.Secrets {
		if seen[s.Name] {
			return fmt.Errorf("binding secrets[%d] duplicates name %q", i, s.Name)
		}
		seen[s.Name] = true
		if err := validateSecret(s); err != nil {
			return fmt.Errorf("binding secret %q: %w", s.Name, err)
		}
	}
	for slot := range b.Config {
		if _, ok := KB310ConfigSlotVariables[slot]; !ok {
			return fmt.Errorf("binding config slot %q is not one of the platform config keys (buildCmd, testCmd, ciGateCmd, baseBranch)", slot)
		}
		if b.Config[slot] == "" {
			return fmt.Errorf("binding config.%s must be a string value", slot)
		}
	}
	return nil
}

// ValidateBinding checks a binding payload's apiVersion/kind and shape.
func ValidateBinding(b *Binding) error {
	if b.APIVersion != APIVersion {
		return fmt.Errorf("binding apiVersion must be %q, got %q", APIVersion, b.APIVersion)
	}
	if b.Kind != KindBinding {
		return fmt.Errorf("binding kind must be %q, got %q", KindBinding, b.Kind)
	}
	return (BindingRules{Images: b.Images, Env: b.Env, Volumes: b.Volumes, Secrets: b.Secrets, Config: b.Config}).Validate()
}
