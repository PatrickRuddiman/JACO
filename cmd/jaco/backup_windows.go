package main

import "os"

func checkBackupDirectory(os.FileInfo) error {
	// Unix mode bits do not model Windows ACLs; operators must use a private ACL.
	return nil
}
