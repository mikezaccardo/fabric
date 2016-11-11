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

	// Bob is the administrator
	bob     crypto.Client
	bobCert crypto.CertificateHandler

	// Charlie and Dave are owners
	charlie     crypto.Client
	charlieCert crypto.CertificateHandler
)

func assignOwnership() (err error) {
	appLogger.Debug("------------- Bob wants to assign the asset 'Picasso' to Charlie...")

	// 1. Bob is the administrator of the chaincode;
	// 2. Bob wants to assign the asset 'Picasso' to Charlie;
	// 3. Bob obtains, via an out-of-band channel, a TCert of Charlie, let us call this certificate *CharlieCert*;

	bobCert, err = bob.GetTCertificateHandlerNext()
	if err != nil {
		appLogger.Errorf("Failed getting Bob TCert [%s]", err)
		return
	}

  // Administrator assigns ownership of Picasso to Charlie
	charlieCert, err = charlie.GetTCertificateHandlerNext()
	if err != nil {
		appLogger.Errorf("Failed getting Charlie TCert [%s]", err)
		return
	}

	// 4. Bob constructs an execute transaction, as described in *application-ACL.md*, to invoke the *assign*
	// function passing as parameters *('Picasso', DER(CharlieCert))*.
	// 5. Bob submits the transaction to the fabric network.

	if bob == nil {
    appLogger.Error("bob is nil")
  }

  if bobCert == nil {
    appLogger.Error("bobCert is nil")
  }

  if charlieCert == nil {
    appLogger.Error("charlieCert is nil")
  }

  resp, err := assignOwnershipInternal(bob, bobCert, "Picasso", charlieCert)
	if err != nil {
		appLogger.Errorf("Failed assigning ownership [%s]", err)
		return
	}
	appLogger.Debugf("Resp [%s]", resp.String())

	appLogger.Debug("Wait 60 seconds")
	time.Sleep(60 * time.Second)

	// Check the owner of 'Picasso". It should be charlie
	appLogger.Debug("Query....")
	queryTx, theOwnerIs, err := whoIsTheOwner(bob, "Picasso")
	if err != nil {
		return
	}
	appLogger.Debugf("Resp [%s]", theOwnerIs.String())
	appLogger.Debug("Query....done")

	var res []byte
	if confidentialityOn {
		// Decrypt result
		res, err = bob.DecryptQueryResult(queryTx, theOwnerIs.Msg)
		if err != nil {
			appLogger.Errorf("Failed decrypting result [%s]", err)
			return
		}
	} else {
		res = theOwnerIs.Msg
	}

	if !reflect.DeepEqual(res, charlieCert.GetCertificate()) {
		appLogger.Error("Charlie is not the owner.")

		appLogger.Debugf("Query result  : [% x]", res)
		appLogger.Debugf("Charlie's cert: [% x]", charlieCert.GetCertificate())

		return fmt.Errorf("Charlie is not the owner.")
	}
	appLogger.Debug("Charlie is the owner!")

	appLogger.Debug("Wait 60 seconds...")
	time.Sleep(60 * time.Second)

	appLogger.Debug("------------- Done!")
	return
}

func testAssetManagementChaincode() (err error) {
	// Assign
	err = assignOwnership()
	if err != nil {
		appLogger.Errorf("Failed assigning ownership [%s]", err)
		return
	}

	appLogger.Debug("Assigned ownership!")

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

	chaincodeName = "2b2d6b91c221c837811d43dfc3f071d6ddd41fc48573135992b8f04de8ee3c96ab4d9cfa50ed2d64ed4e87b2998c9239f34d8e9132ca7fd6a9bd372e4415bed5"

  // Exercise the 'asset_management' chaincode
	if err := testAssetManagementChaincode(); err != nil {
		appLogger.Debugf("Failed testing asset management chaincode [%s]", err)
		os.Exit(-2)
	}
}
