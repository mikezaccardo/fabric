/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/hyperledger/fabric/core/crypto"
	pb "github.com/hyperledger/fabric/protos"
	"github.com/op/go-logging"
	"google.golang.org/grpc"
)

var (
	// Logging
	appLogger = logging.MustGetLogger("app")

	// NVP related objects
	peerClientConn *grpc.ClientConn
	serverClient   pb.PeerClient

	// Charlie and Dave are owners
	charlie     crypto.Client
	charlieCert crypto.CertificateHandler

	dave     crypto.Client
	daveCert crypto.CertificateHandler
)

func transferOwnership() (err error) {
	appLogger.Debug("------------- Charlie wants to transfer the ownership of 'Picasso' to Dave...")

	// 1. Charlie is the owner of 'Picasso';
	// 2. Charlie wants to transfer the ownership of 'Picasso' to Dave;
	// 3. Charlie obtains, via an out-of-band channel, a TCert of Dave, let us call this certificate *DaveCert*;
	charlieCert, err = charlie.GetEnrollmentCertificateHandler()
	if err != nil {
		appLogger.Errorf("Failed getting Charlie TCert [%s]", err)
		return
	}

  daveCert, err = dave.GetEnrollmentCertificateHandler()
	if err != nil {
		appLogger.Errorf("Failed getting Dave TCert [%s]", err)
		return
	}

	// 4. Charlie constructs an execute transaction, as described in *application-ACL.md*, to invoke the *transfer*
	// function passing as parameters *('Picasso', DER(DaveCert))*.
	// 5. Charlie submits the transaction to the fabric network.

	resp, err := transferOwnershipInternal(charlie, charlieCert, "Picasso", daveCert)
	if err != nil {
		return
	}
	appLogger.Debugf("Resp [%s]", resp.String())

	appLogger.Debug("Wait 60 seconds")
	time.Sleep(60 * time.Second)

	appLogger.Debug("Query....")
	queryTx, theOwnerIs, err := whoIsTheOwner(charlie, "Picasso")
	if err != nil {
		return
	}
	appLogger.Debugf("Resp [%s]", theOwnerIs.String())
	appLogger.Debug("Query....done")

	var res []byte
	if confidentialityOn {
		// Decrypt result
		res, err = charlie.DecryptQueryResult(queryTx, theOwnerIs.Msg)
		if err != nil {
			appLogger.Errorf("Failed decrypting result [%s]", err)
			return
		}
	} else {
		res = theOwnerIs.Msg
	}

	if !reflect.DeepEqual(res, daveCert.GetCertificate()) {
		appLogger.Error("Dave is not the owner.")

		appLogger.Debugf("Query result  : [% x]", res)
		appLogger.Debugf("Dave's cert: [% x]", daveCert.GetCertificate())

		return fmt.Errorf("Dave is not the owner.")
	}

	appLogger.Debug("------------- Done!")
	return
}

func testAssetManagementChaincode() (err error) {
	// Transfer
	err = transferOwnership()
	if err != nil {
		appLogger.Errorf("Failed transfering ownership [%s]", err)
		return
	}

	appLogger.Debug("Dave is the owner!")

	return
}

func main() {
	// Initialize a non-validating peer whose role is to submit
	// transactions to the fabric network.
	// A 'core.yaml' file is assumed to be available in the working directory.
	if err := initNVP(); err != nil {
		appLogger.Debugf("Failed initiliazing NVP [%s]", err)
		os.Exit(-1)
	}

	// Enable fabric 'confidentiality'
	confidentiality(true)

  chaincodeName = os.Args[1]

  // Exercise the 'asset_management' chaincode
	if err := testAssetManagementChaincode(); err != nil {
		appLogger.Debugf("Failed testing asset management chaincode [%s]", err)
		os.Exit(-2)
	}
}
