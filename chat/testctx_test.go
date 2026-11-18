package chat

import "context"

// ctx is the shared background context of the package's tests.
//
// It used to live in memstore_test.go. That file is now an external test
// (package chat_test) because it imports chat/storetest, which imports chat;
// an in-package test cannot import a package that imports the package under
// test. This keeps these helpers available to the remaining in-package tests.
func ctx() context.Context { return context.Background() }

// otherOwner is the fixture owner of a different user in the same partition.
func otherOwner() OwnerKey {
	k := ownerFixture()
	k.UserID = "otheruser000001"
	return k
}
