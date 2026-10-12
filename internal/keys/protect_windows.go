//go:build windows

package keys

import "golang.org/x/sys/windows"

// Protect gives a private key a permission list of its own: its user,
// SYSTEM and the Administrators, and nothing inherited from its folder.
//
// On Windows a file's mode is not its permissions: a new file takes its
// folder's, and whatever is later granted on the folder reaches the file
// too. Codex's sandbox grants a group of its own the right to read most
// folders at the top of the home folder, the state folder among them, and
// Windows' OpenSSH refuses a private key anyone but its owner can read
// ("UNPROTECTED PRIVATE KEY FILE"), so `exe ssh` lost its key on any PC
// where Codex had run. A list of the file's own, with inheritance off,
// is what OpenSSH asks for and what a later grant on the folder cannot
// reach.
func Protect(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, sid := range []*windows.SID{user.User.Sid, system, admins} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	// PROTECTED is what turns inheritance off: the list is these three
	// entries and nothing from above
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}
