package seccomp

// OASIS SARIF v2.1.0 schema structs for GitHub Code Scanning integration.

const sarifSchemaURI = "https://json.schemastore.org/sarif-2.1.0.json"

// SarifLog is the top-level OASIS SARIF v2.1.0 document.
type SarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []SarifRun `json:"runs"`
}

// SarifRun represents a single static analysis tool execution.
type SarifRun struct {
	Tool        SarifTool         `json:"tool"`
	Invocations []SarifInvocation `json:"invocations"`
	Results     []SarifResult     `json:"results"`
}

// SarifTool describes OmniVM's seccomp auditor driver and rule catalog.
type SarifTool struct {
	Driver SarifDriver `json:"driver"`
}

// SarifDriver holds tool metadata and reporting descriptors.
type SarifDriver struct {
	Name           string                `json:"name"`
	Version        string                `json:"version"`
	InformationURI string                `json:"informationUri"`
	Rules          []SarifRuleDescriptor `json:"rules"`
}

// SarifRuleDescriptor defines one rule in tool.driver.rules.
type SarifRuleDescriptor struct {
	ID                   string             `json:"id"`
	Name                 string             `json:"name"`
	ShortDescription     SarifMessage       `json:"shortDescription"`
	Help                 SarifMessage       `json:"help"`
	DefaultConfiguration SarifDefaultConfig `json:"defaultConfiguration"`
	Properties           map[string]string  `json:"properties,omitempty"`
}

// SarifDefaultConfig specifies default SARIF level ("error", "warning", "note").
type SarifDefaultConfig struct {
	Level string `json:"level"`
}

// SarifMessage wraps a plain-text message string.
type SarifMessage struct {
	Text string `json:"text"`
}

// SarifInvocation records execution success status.
type SarifInvocation struct {
	ExecutionSuccessful bool `json:"executionSuccessful"`
}

// SarifResult represents a single SARIF diagnostic result.
type SarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             SarifMessage      `json:"message"`
	Locations           []SarifLocation   `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
}

// SarifLocation points to the audited seccomp profile artifact.
type SarifLocation struct {
	PhysicalLocation SarifPhysicalLocation `json:"physicalLocation"`
}

// SarifPhysicalLocation identifies the artifact URI and region.
type SarifPhysicalLocation struct {
	ArtifactLocation SarifArtifactLocation `json:"artifactLocation"`
	Region           *SarifRegion          `json:"region,omitempty"`
}

// SarifArtifactLocation holds the relative or logical file URI.
type SarifArtifactLocation struct {
	URI string `json:"uri"`
}

// SarifRegion provides a 1-indexed line anchor for GitHub Code Scanning.
type SarifRegion struct {
	StartLine int `json:"startLine"`
}

// ToSARIF converts an AuditReport into an OASIS SARIF v2.1.0 document.
func ToSARIF(rep *AuditReport, toolVersion string) *SarifLog {
	if toolVersion == "" {
		toolVersion = "0.8.0"
	}
	rules := make([]SarifRuleDescriptor, len(Catalog))
	ruleIdx := make(map[string]int, len(Catalog))
	for i, r := range Catalog {
		ruleIdx[r.ID] = i
		rules[i] = SarifRuleDescriptor{
			ID:               r.ID,
			Name:             r.Name,
			ShortDescription: SarifMessage{Text: r.Summary},
			Help:             SarifMessage{Text: r.Summary + " Remediation: " + r.Remediation},
			DefaultConfiguration: SarifDefaultConfig{
				Level: r.DefaultSeverity,
			},
			Properties: map[string]string{
				"security-severity": r.SecuritySeverity,
				"tags":              "security,seccomp,microvm,kvm",
			},
		}
	}

	var results []SarifResult
	if rep != nil {
		results = make([]SarifResult, 0, len(rep.Findings))
		uri := rep.SourcePath
		if uri == "" {
			uri = "seccomp.json"
		}
		for _, f := range rep.Findings {
			idx := ruleIdx[f.RuleID]
			results = append(results, SarifResult{
				RuleID:    f.RuleID,
				RuleIndex: idx,
				Level:     f.Severity,
				Message:   SarifMessage{Text: f.Message + " Remediation: " + f.Remediation},
				Locations: []SarifLocation{
					{
						PhysicalLocation: SarifPhysicalLocation{
							ArtifactLocation: SarifArtifactLocation{URI: uri},
							Region:           &SarifRegion{StartLine: 1},
						},
					},
				},
				PartialFingerprints: map[string]string{
					"primaryLocationLineHash": f.Fingerprint,
				},
			})
		}
	}
	if results == nil {
		results = []SarifResult{}
	}

	return &SarifLog{
		Schema:  sarifSchemaURI,
		Version: "2.1.0",
		Runs: []SarifRun{
			{
				Tool: SarifTool{
					Driver: SarifDriver{
						Name:           "omnivm-seccomp-auditor",
						Version:        toolVersion,
						InformationURI: "https://github.com/SalyyS1/omnivm",
						Rules:          rules,
					},
				},
				Invocations: []SarifInvocation{{ExecutionSuccessful: true}},
				Results:     results,
			},
		},
	}
}
