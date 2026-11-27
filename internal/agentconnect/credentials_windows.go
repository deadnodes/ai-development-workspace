package agentconnect

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func makeCredentialDir(root *os.Root, name string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return err
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(root.Name(), name))
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	return windows.CreateDirectory(path, &attributes)
}

func checkCredentialAccess(root *os.Root, name string, info os.FileInfo) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	handle := windows.Handle(file.Fd())
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		return err
	}
	if handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("credential path must not be a Windows reparse point")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !(owner.Equals(user.User.Sid) || (!info.IsDir() && owner.IsWellKnown(windows.WinBuiltinAdministratorsSid))) {
		return errors.New("credential path must be owned by the current Windows user")
	}
	control, _, err := descriptor.Control()
	if err != nil || info.IsDir() && control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("credential directory must have an explicit private Windows ACL")
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		return errors.New("credential path must grant access only to the current Windows user")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		return err
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user.User.Sid) {
		return errors.New("credential path must grant access only to the current Windows user")
	}
	if info.IsDir() && ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
		return errors.New("credential directory must pass its private ACL to new files")
	}
	return nil
}
