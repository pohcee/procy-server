package users

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	PermissionTunnels    = "tunnels"
	PermissionCommands   = "commands"
	PermissionVault      = "vault"
	PermissionScheduler  = "scheduler"
	PermissionMonitoring = "monitoring"
	PermissionUploads    = "uploads"
	PermissionsAuditLog  = "auditlog"

	// PermissionScripts is deprecated: it is merged into PermissionCommands.
	// Stored groups may still carry it, so it is honoured when read.
	PermissionScripts = "scripts"
)

var AllPermissions = []string{
	PermissionTunnels,
	PermissionCommands,
	PermissionVault,
	PermissionScheduler,
	PermissionMonitoring,
	PermissionUploads,
	PermissionsAuditLog,
}

type Permissions struct {
	data map[string]bool
}

func NewPermissions(perms ...string) Permissions {
	permissions := Permissions{
		data: make(map[string]bool),
	}
	for _, p := range perms {
		permissions.data[p] = true
	}
	return permissions
}

func (permissions Permissions) All() map[string]bool {
	result := make(map[string]bool)
	for _, p := range AllPermissions {
		result[p] = permissions.Has(p)
	}
	return result
}

func (permissions Permissions) Has(p string) bool {
	if permissions.data == nil {
		return false
	}
	if p == PermissionCommands || p == PermissionScripts {
		// One right covers both; stored data may hold either key.
		return permissions.data[PermissionCommands] || permissions.data[PermissionScripts]
	}
	return permissions.data[p]
}

func (permissions *Permissions) Scan(value interface{}) error {
	if permissions == nil {
		return errors.New("'permissions' cannot be nil")
	}

	var err error
	switch d := value.(type) {
	case string:
		// Handle sqlite json decoding
		if d == "" {
			return nil
		}
		err = json.Unmarshal([]byte(d), &permissions.data)
	case []uint8:
		// Handle MySQL json decoding
		if len(d) == 0 {
			return nil
		}
		err = json.Unmarshal(d, &permissions.data)
	default:
		return fmt.Errorf("failed to decode json column: unknown comlumn type %T", value)
	}

	if err != nil {
		return fmt.Errorf("failed to decode 'permissions' field: %v", err)
	}
	// Value stores "scripts" next to "commands" for older rportd versions; it
	// carries no extra information once both are present.
	if permissions.data[PermissionCommands] {
		delete(permissions.data, PermissionScripts)
	}
	return nil
}

// Value serialises the permissions for storage. The deprecated "scripts" key
// is written whenever "commands" is set, so an older rportd reading the same
// group still grants the scripts right.
func (permissions Permissions) Value() (driver.Value, error) {
	data := make(map[string]bool, len(permissions.data)+1)
	for k, v := range permissions.data {
		data[k] = v
	}
	if data[PermissionCommands] {
		data[PermissionScripts] = true
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to encode 'permissions' field: %v", err)
	}
	return string(b), nil
}

func (permissions Permissions) MarshalJSON() ([]byte, error) {
	return json.Marshal(permissions.All())
}

func (permissions *Permissions) UnmarshalJSON(data []byte) error {
	result := make(map[string]bool)
	err := json.Unmarshal(data, &result)
	if err != nil {
		return err
	}

	for key := range result {
		isPermission := key == PermissionScripts
		for _, permission := range AllPermissions {
			if key == permission {
				isPermission = true
				break
			}
		}
		if !isPermission {
			return fmt.Errorf("invalid permission: %v", key)
		}
	}

	if result[PermissionScripts] {
		result[PermissionCommands] = true
	}
	delete(result, PermissionScripts)

	permissions.data = result
	return nil
}
