// This go.mod file is generated by ./hack/codegen.sh.
module go.pinniped.dev/generated/1.17/client

go 1.13

require (
	go.pinniped.dev/generated/1.17/apis v0.0.0
	k8s.io/apimachinery v0.17.11
	k8s.io/client-go v0.17.11
)

replace go.pinniped.dev/generated/1.17/apis => ../apis
