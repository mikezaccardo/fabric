go get -u github.com/gorilla/mux

cd $APP_HOME
mkdir app1 app2 app3

cd app1
cp ../core.yaml .
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app1/app1.go
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app1/app1_internal.go
go build

cd ../app2
cp ../core.yaml .
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app2/app2.go
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app2/app2_internal.go
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app2/assets.txt
go build

cd ../app3
cp ../core.yaml .
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app3/app3.go
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app3/app3_internal.go
curl -O https://raw.githubusercontent.com/mikezaccardo/fabric/interconnect-demo/examples/chaincode/go/asset_management_interactive/app3/assets.txt
go build

cd ..
