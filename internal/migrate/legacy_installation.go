// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// These bounds also apply before decoding, not only to the decoded document.
	LegacyInstallationMaxYAMLBytes = 1 << 20
	legacyInstallationMaxDepth     = 32
	legacyInstallationMaxNodes     = 32768

	LegacySettingsSourceYAML        = "config-yaml"
	LegacySettingsSourceCompose     = "config-yaml+compose-literals"
	LegacySettingsSourceComposeOnly = "compose-literals"
)

// LegacyInstallation is private migration input, never a discovery response.
// EffectiveYAML contains source credentials and must be handled like Password.
// The coordinator, not this parser, proves the selected files and config mount
// belong to the same installation and obtains runtime-settings confirmation.
type LegacyInstallation struct {
	Driver                      string   `json:"-"`
	Host                        string   `json:"-"`
	Port                        int      `json:"-"`
	Database                    string   `json:"-"`
	User                        string   `json:"-"`
	Password                    string   `json:"-"`
	EffectiveYAML               []byte   `json:"-"`
	ConfigMount                 string   `json:"-"`
	SettingsSource              string   `json:"-"`
	Warnings                    []string `json:"-"`
	RequiresRuntimeConfirmation bool     `json:"-"`
}

// String and GoString prevent ordinary diagnostic formatting from disclosing
// this struct's credentials. Callers must not log individual input fields.
func (LegacyInstallation) String() string   { return "LegacyInstallation{private settings}" }
func (LegacyInstallation) GoString() string { return "LegacyInstallation{private settings}" }

// ParseLegacyInstallation resolves actual config.yml and co-located original
// docker-compose.yml bytes (nil means absent for either file). An absent config
// is supported only with a positively identified Compose containing every
// explicit connection setting. It never executes Compose, reads a file,
// consults the process environment, expands placeholders, or uses .env.
// Config/example identity and filesystem anchoring are the caller's concern.
// Errors and warnings contain only static diagnostics, never supplied values.
func ParseLegacyInstallation(configYAML, composeYAML []byte) (LegacyInstallation, error) {
	var config *yaml.Node
	var err error
	if configYAML == nil {
		if composeYAML == nil {
			return LegacyInstallation{}, errors.New("original installation requires actual config or explicit original Compose settings")
		}
		config = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	} else {
		config, err = parseInstallationYAML(configYAML)
		if err != nil {
			return LegacyInstallation{}, errors.New("original config YAML is invalid, ambiguous, or exceeds safety bounds")
		}
	}
	result := LegacyInstallation{
		SettingsSource:              LegacySettingsSourceYAML,
		RequiresRuntimeConfirmation: true,
		Warnings:                    []string{"Confirm these settings match the original running service; config and Compose files cannot prove the absence of runtime overrides"},
	}
	if composeYAML != nil {
		compose, err := parseInstallationYAML(composeYAML)
		if err != nil {
			return LegacyInstallation{}, errors.New("original Compose YAML is invalid, ambiguous, or exceeds safety bounds")
		}
		env, mount, err := originalComposeEnvironment(compose)
		if err != nil {
			return LegacyInstallation{}, err
		}
		if configYAML == nil && !originalDatabaseEnvironmentComplete(env) {
			return LegacyInstallation{}, errors.New("absent original config requires every connection setting explicitly in Compose")
		}
		ignored, err := applyOriginalEnvironment(config, env)
		if err != nil {
			return LegacyInstallation{}, err
		}
		result.ConfigMount = mount
		result.SettingsSource = LegacySettingsSourceCompose
		if configYAML == nil {
			result.SettingsSource = LegacySettingsSourceComposeOnly
		}
		if ignored {
			result.Warnings = append(result.Warnings, "Unrelated literal application environment settings were not translated")
		}
		// The pinned original defaults only its driver to mysql. Accept that
		// default solely when the identified original Compose provides every
		// connection field explicitly. Never synthesize connection credentials.
		db := installationMapValue(config, "database")
		if db != nil && installationMapValue(db, "type") == nil && originalDatabaseEnvironmentComplete(env) {
			installationSetValue(db, "type", installationString("mysql"))
		}
	}
	if err := resolveOriginalDatabase(config, &result); err != nil {
		return LegacyInstallation{}, err
	}
	// Project exactly the ASCII/IP host that the source connector will use.
	// In particular, never expose a misplaced DSN or choose a different IDNA
	// mapping from the original Python runtime.
	result.Host, err = canonicalDetectedHost(result.Host)
	if err != nil {
		return LegacyInstallation{}, err
	}
	if err := validateDetectedSourceConfig(DetectedSourceConfig{Driver: result.Driver, Host: result.Host, Port: result.Port, User: result.User, Password: result.Password, Database: result.Database}); err != nil {
		return LegacyInstallation{}, errors.New("original database requires bounded literal endpoint settings, not a URL or connection string")
	}
	result.EffectiveYAML, err = yaml.Marshal(config)
	if err != nil || len(result.EffectiveYAML) > LegacyInstallationMaxYAMLBytes {
		return LegacyInstallation{}, errors.New("effective original config exceeds safety bounds")
	}
	return result, nil
}

func parseInstallationYAML(data []byte) (*yaml.Node, error) {
	if len(data) == 0 || len(data) > LegacyInstallationMaxYAMLBytes || !utf8.Valid(data) {
		return nil, errors.New("YAML byte limit")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil || len(document.Content) != 1 {
		return nil, errors.New("invalid YAML document")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("multiple YAML documents")
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("YAML root is not a mapping")
	}
	nodes := 0
	if err := validateInstallationYAML(root, 0, &nodes); err != nil {
		return nil, err
	}
	return root, nil
}

func validateInstallationYAML(node *yaml.Node, depth int, nodes *int) error {
	(*nodes)++
	if depth > legacyInstallationMaxDepth || *nodes > legacyInstallationMaxNodes {
		return errors.New("YAML structure limit")
	}
	if node.Anchor != "" || node.Alias != nil || node.Kind == yaml.AliasNode {
		return errors.New("YAML anchors and aliases are unsupported")
	}
	switch node.Kind {
	case yaml.MappingNode:
		if node.Tag != "!!map" || len(node.Content)%2 != 0 {
			return errors.New("unsupported YAML mapping")
		}
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || seen[key.Value] {
				return errors.New("ambiguous YAML key")
			}
			seen[key.Value] = true
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return errors.New("unsupported YAML sequence")
		}
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str", "!!int", "!!bool", "!!float", "!!null":
		default:
			return errors.New("unsupported YAML tag")
		}
	default:
		return errors.New("unsupported YAML node")
	}
	for _, child := range node.Content {
		if err := validateInstallationYAML(child, depth+1, nodes); err != nil {
			return err
		}
	}
	return nil
}

func installationMapValue(node *yaml.Node, name string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == name {
			return node.Content[i+1]
		}
	}
	return nil
}

func installationSetValue(node *yaml.Node, name string, value *yaml.Node) {
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == name {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, installationString(name), value)
}

func installationString(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func installationLiteralString(node *yaml.Node) (string, bool) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", false
	}
	return node.Value, true
}

// Any dollar in a Compose scalar is rejected, including $$: Compose escaping is
// deliberately outside this inert parser's contract. Config YAML is not Compose
// and may contain literal dollars in secrets without changing their meaning.
func originalComposeLiteral(node *yaml.Node) bool {
	if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, "$") {
		return false
	}
	for _, child := range node.Content {
		if !originalComposeLiteral(child) {
			return false
		}
	}
	return true
}

func originalComposeEnvironment(root *yaml.Node) (map[string]string, string, error) {
	invalid := errors.New("Compose does not unambiguously describe the supported original application and config bind")
	if !originalComposeLiteral(root) {
		return nil, "", errors.New("Compose interpolation and dollar escapes are unsupported; provide literal original settings")
	}
	for i := 0; i < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "services", "version", "name", "volumes", "networks":
		default:
			return nil, "", invalid
		}
	}
	services := installationMapValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode || len(services.Content) == 0 || len(services.Content) > 64 {
		return nil, "", invalid
	}
	app := installationMapValue(services, "app")
	if app == nil || app.Kind != yaml.MappingNode {
		return nil, "", invalid
	}
	for i := 0; i < len(services.Content); i += 2 {
		service := services.Content[i+1]
		if service.Kind != yaml.MappingNode {
			return nil, "", invalid
		}
		for _, name := range []string{"env_file", "extends", "include"} {
			if installationMapValue(service, name) != nil {
				return nil, "", errors.New("Compose external settings and included service definitions are unsupported")
			}
		}
		if services.Content[i].Value != "app" && originalComposeIdentity(service) {
			return nil, "", errors.New("Compose contains multiple possible original application services")
		}
	}
	for _, name := range []string{"env_file", "extends", "include", "command", "entrypoint", "working_dir", "configs", "secrets", "profiles"} {
		if installationMapValue(app, name) != nil {
			return nil, "", errors.New("Compose external settings, alternate entrypoints, and conditional services are unsupported")
		}
	}
	container, ok := installationLiteralString(installationMapValue(app, "container_name"))
	if !ok || container != "danmu-api" || !originalComposeCode(app) {
		return nil, "", invalid
	}
	mount, err := originalComposeConfigMount(installationMapValue(app, "volumes"))
	if err != nil {
		return nil, "", err
	}
	env, err := originalComposeLiteralEnvironment(installationMapValue(app, "environment"))
	if err != nil {
		return nil, "", err
	}
	// An original database setting is part of identifying this service; a
	// matching container name by itself is not sufficient evidence. A partial
	// environment can override a complete config, but cannot infer its driver.
	identified := false
	for _, key := range []string{"TYPE", "HOST", "PORT", "USER", "PASSWORD", "NAME"} {
		if _, ok := env["DANMUAPI_DATABASE__"+key]; ok {
			identified = true
		}
	}
	if !identified {
		return nil, "", invalid
	}
	return env, mount, nil
}

func originalComposeIdentity(service *yaml.Node) bool {
	if originalComposeCode(service) {
		return true
	}
	if name, _ := installationLiteralString(installationMapValue(service, "container_name")); name == "danmu-api" {
		return true
	}
	if image, _ := installationLiteralString(installationMapValue(service, "image")); strings.HasPrefix(image, "l429609201/danmu_api_server") {
		return true
	}
	env := installationMapValue(service, "environment")
	if env == nil {
		return false
	}
	if env.Kind == yaml.MappingNode {
		for i := 0; i < len(env.Content); i += 2 {
			if strings.HasPrefix(strings.ToUpper(env.Content[i].Value), "DANMUAPI_") {
				return true
			}
		}
	}
	if env.Kind == yaml.SequenceNode {
		for _, item := range env.Content {
			if strings.HasPrefix(strings.ToUpper(item.Value), "DANMUAPI_") {
				return true
			}
		}
	}
	return false
}

func originalComposeCode(app *yaml.Node) bool {
	build, image := installationMapValue(app, "build"), installationMapValue(app, "image")
	if (build == nil) == (image == nil) {
		return false
	}
	if build != nil {
		if value, ok := installationLiteralString(build); ok {
			return value == "." || value == "./"
		}
		if build.Kind != yaml.MappingNode || len(build.Content) != 2 || build.Content[0].Value != "context" {
			return false
		}
		value, ok := installationLiteralString(build.Content[1])
		return ok && (value == "." || value == "./")
	}
	value, ok := installationLiteralString(image)
	if !ok {
		return false
	}
	const official = "l429609201/danmu_api_server"
	if value == official {
		return true
	}
	if strings.HasPrefix(value, official+":") {
		tag := strings.TrimPrefix(value, official+":")
		if len(tag) == 0 || len(tag) > 128 {
			return false
		}
		for _, char := range tag {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '.' || char == '-') {
				return false
			}
		}
		return true
	}
	if strings.HasPrefix(value, official+"@sha256:") {
		digest := strings.TrimPrefix(value, official+"@sha256:")
		if len(digest) != 64 {
			return false
		}
		for _, char := range digest {
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
				return false
			}
		}
		return true
	}
	return false
}

func originalComposeConfigMount(volumes *yaml.Node) (string, error) {
	invalid := errors.New("Compose must contain one documented relative config bind without overlapping mounts")
	if volumes == nil || volumes.Kind != yaml.SequenceNode || len(volumes.Content) != 1 {
		return "", invalid
	}
	volume, ok := installationLiteralString(volumes.Content[0])
	if !ok {
		return "", invalid
	}
	parts := strings.Split(volume, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "./config" || parts[1] != "/app/config" {
		return "", invalid
	}
	if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
		return "", invalid
	}
	return "./config", nil
}

func originalComposeLiteralEnvironment(node *yaml.Node) (map[string]string, error) {
	invalid := errors.New("Compose application environment must contain unique explicit literal values")
	if node == nil {
		return nil, invalid
	}
	result := make(map[string]string)
	add := func(key, value string) error {
		if key == "" || strings.ContainsAny(key, "= \t\r\n\x00") || strings.ContainsRune(value, '\x00') {
			return invalid
		}
		key = strings.ToUpper(key)
		if _, exists := result[key]; exists {
			return invalid
		}
		result[key] = value
		return nil
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			value := node.Content[i+1]
			// Compose permits numeric mapping values. Null is pass-through,
			// and booleans/collections are not unambiguous string settings.
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" && value.Tag != "!!int" {
				return nil, invalid
			}
			if err := add(node.Content[i].Value, value.Value); err != nil {
				return nil, err
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			value, ok := installationLiteralString(item)
			if !ok {
				return nil, invalid
			}
			key, value, ok := strings.Cut(value, "=")
			if !ok {
				return nil, invalid
			}
			if err := add(key, value); err != nil {
				return nil, err
			}
		}
	default:
		return nil, invalid
	}
	return result, nil
}

func originalDatabaseEnvironmentComplete(env map[string]string) bool {
	for _, key := range []string{"HOST", "PORT", "USER", "PASSWORD", "NAME"} {
		if _, ok := env["DANMUAPI_DATABASE__"+key]; !ok {
			return false
		}
	}
	return true
}

// Only fields understood by migration's legacy-config translator are overlaid.
// Complex parent JSON settings or unknown database fields can affect the source
// connection, so fail closed instead of silently relying on the YAML values.
func applyOriginalEnvironment(root *yaml.Node, env map[string]string) (bool, error) {
	fields := map[string]string{
		"DATABASE__TYPE": "str", "DATABASE__HOST": "str", "DATABASE__PORT": "int",
		"DATABASE__USER": "str", "DATABASE__PASSWORD": "str", "DATABASE__NAME": "str",
		"JWT__SECRET_KEY": "str", "JWT__ALGORITHM": "str", "JWT__ACCESS_TOKEN_EXPIRE_MINUTES": "int",
		"ADMIN__INITIAL_USER": "str", "ADMIN__INITIAL_PASSWORD": "str", "TZ": "str",
		"SERVER__HOST": "str", "SERVER__PORT": "int", "SERVER__IPV6": "bool",
		"CACHE__BACKEND": "str", "CACHE__REDIS_URL": "str", "CACHE__REDIS_MAX_MEMORY": "str",
		"CACHE__REDIS_SOCKET_TIMEOUT": "int", "CACHE__REDIS_SOCKET_CONNECT_TIMEOUT": "int",
		"CACHE__MEMORY_MAXSIZE": "int", "CACHE__MEMORY_DEFAULT_TTL": "int",
	}
	ignored := false
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := env[key]
		if !strings.HasPrefix(key, "DANMUAPI_") {
			continue
		}
		field := strings.TrimPrefix(key, "DANMUAPI_")
		kind, ok := fields[field]
		if !ok {
			for _, section := range []string{"DATABASE", "JWT", "ADMIN", "SERVER", "CACHE", "TZ"} {
				if field == section || strings.HasPrefix(field, section+"__") {
					return false, errors.New("Compose contains an unsupported environment override for original migration settings")
				}
			}
			ignored = true
			continue
		}
		node := installationString(value)
		switch kind {
		case "int":
			integer, err := strconv.Atoi(value)
			if err != nil {
				return false, errors.New("Compose original integer environment setting is invalid")
			}
			node.Tag, node.Value = "!!int", strconv.Itoa(integer)
		case "bool":
			switch strings.ToLower(value) {
			case "true", "1", "yes", "on":
				node.Tag, node.Value = "!!bool", "true"
			case "false", "0", "no", "off":
				node.Tag, node.Value = "!!bool", "false"
			default:
				return false, errors.New("Compose original boolean environment setting is invalid")
			}
		}
		section, name, nested := strings.Cut(strings.ToLower(field), "__")
		if !nested {
			installationSetValue(root, section, node)
			continue
		}
		parent := installationMapValue(root, section)
		if parent == nil {
			parent = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			installationSetValue(root, section, parent)
		}
		if parent.Kind != yaml.MappingNode {
			return false, errors.New("original config section is not a mapping")
		}
		installationSetValue(parent, name, node)
	}
	return ignored, nil
}

func resolveOriginalDatabase(root *yaml.Node, result *LegacyInstallation) error {
	invalid := errors.New("original database requires explicit mysql/postgresql type, host, port, user, password, and name")
	db := installationMapValue(root, "database")
	if db == nil || db.Kind != yaml.MappingNode {
		return invalid
	}
	driver, ok := installationLiteralString(installationMapValue(db, "type"))
	if !ok {
		return invalid
	}
	switch strings.ToLower(driver) {
	case "mysql":
		result.Driver = "mysql"
	case "postgres", "postgresql":
		result.Driver = "postgres"
	default:
		return invalid
	}
	for _, field := range []struct {
		name string
		dst  *string
	}{
		{"host", &result.Host}, {"user", &result.User},
		{"password", &result.Password}, {"name", &result.Database},
	} {
		value, ok := installationLiteralString(installationMapValue(db, field.name))
		if !ok || strings.ContainsRune(value, '\x00') || field.name != "password" && strings.TrimSpace(value) == "" {
			return invalid
		}
		*field.dst = value
	}
	port := installationMapValue(db, "port")
	if port == nil || port.Kind != yaml.ScalarNode || port.Tag != "!!int" && port.Tag != "!!str" {
		return invalid
	}
	var err error
	result.Port, err = strconv.Atoi(port.Value)
	if err != nil || result.Port < 1 || result.Port > 65535 {
		return invalid
	}
	// Pydantic accepts a decimal string for this integer field. Preserve its
	// effective integer type for the existing legacy configuration translator.
	installationSetValue(db, "port", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(result.Port)})
	return nil
}
