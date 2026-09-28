package store

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func secureDir(path string) error {
	sid, err := userSIDString()
	if err != nil {
		return err
	}
	dacl := "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	sd, err := windows.SecurityDescriptorFromString(dacl)
	if err != nil {
		return fmt.Errorf("build the owner-only descriptor for %s: %w", path, err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read the owner-only dacl for %s: %w", path, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("set the owner-only dacl on %s: %w", path, err)
	}
	return nil
}

func userSIDString() (string, error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", fmt.Errorf("open the process token: %w", err)
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read the process user: %w", err)
	}
	return tu.User.Sid.String(), nil
}
