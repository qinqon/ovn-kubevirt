// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.

package ovsnb

const ACLTable = "ACL"

type (
	ACLAction    = string
	ACLDirection = string
	ACLSeverity  = string
)

var (
	ACLActionAllow          ACLAction    = "allow"
	ACLActionAllowRelated   ACLAction    = "allow-related"
	ACLActionAllowStateless ACLAction    = "allow-stateless"
	ACLActionDrop           ACLAction    = "drop"
	ACLActionReject         ACLAction    = "reject"
	ACLDirectionFromLport   ACLDirection = "from-lport"
	ACLDirectionToLport     ACLDirection = "to-lport"
	ACLSeverityAlert        ACLSeverity  = "alert"
	ACLSeverityDebug        ACLSeverity  = "debug"
	ACLSeverityInfo         ACLSeverity  = "info"
	ACLSeverityNotice       ACLSeverity  = "notice"
	ACLSeverityWarning      ACLSeverity  = "warning"
)

// ACL defines an object in ACL table
type ACL struct {
	UUID        string            `ovsdb:"_uuid"`
	Action      ACLAction         `ovsdb:"action"`
	Direction   ACLDirection      `ovsdb:"direction"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	Label       int               `ovsdb:"label"`
	Log         bool              `ovsdb:"log"`
	Match       string            `ovsdb:"match"`
	Meter       *string           `ovsdb:"meter"`
	Name        *string           `ovsdb:"name"`
	Options     map[string]string `ovsdb:"options"`
	Priority    int               `ovsdb:"priority"`
	Severity    *ACLSeverity      `ovsdb:"severity"`
}
