package schemas

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed v1/agentfile.schema.json
var AgentfileV1JSON []byte

//go:embed v1/agentfile-config.schema.json
var LocalConfigV1JSON []byte

//go:embed v1/agentfile-lock.schema.json
var LockfileV1JSON []byte

const (
	agentfileSchemaURL   = "https://github.com/bettertux007/agentfile/schemas/v1/agentfile.schema.json"
	localConfigSchemaURL = "https://github.com/bettertux007/agentfile/schemas/v1/agentfile-config.schema.json"
	lockfileSchemaURL    = "https://github.com/bettertux007/agentfile/schemas/v1/agentfile-lock.schema.json"
)

// ValidateAgentfileV1 validates raw YAML bytes against the embedded Agentfile v1 JSON Schema.
func ValidateAgentfileV1(yamlBytes []byte) error {
	return validateYAMLWithSchema(yamlBytes, AgentfileV1JSON, agentfileSchemaURL)
}

// ValidateLocalConfigV1 validates raw YAML bytes against the embedded .agentfile/config.yaml v1 JSON Schema.
func ValidateLocalConfigV1(yamlBytes []byte) error {
	return validateYAMLWithSchema(yamlBytes, LocalConfigV1JSON, localConfigSchemaURL)
}

// ValidateLockfileV1 validates raw YAML bytes against the embedded Agentfile.lock v1 JSON Schema.
func ValidateLockfileV1(yamlBytes []byte) error {
	return validateYAMLWithSchema(yamlBytes, LockfileV1JSON, lockfileSchemaURL)
}

func validateYAMLWithSchema(yamlBytes, schemaJSON []byte, url string) error {
	var doc any
	if err := yaml.Unmarshal(yamlBytes, &doc); err != nil {
		return fmt.Errorf("parse YAML for schema check: %w", err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return fmt.Errorf("load embedded schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, schemaDoc); err != nil {
		return fmt.Errorf("register embedded schema: %w", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		return fmt.Errorf("compile embedded schema: %w", err)
	}
	return sch.Validate(doc)
}
