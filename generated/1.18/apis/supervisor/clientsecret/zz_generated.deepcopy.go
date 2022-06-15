//go:build !ignore_autogenerated
// +build !ignore_autogenerated

// Copyright 2020-2022 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Code generated by deepcopy-gen. DO NOT EDIT.

package clientsecret

import (
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *OIDCClientSecretRequest) DeepCopyInto(out *OIDCClientSecretRequest) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new OIDCClientSecretRequest.
func (in *OIDCClientSecretRequest) DeepCopy() *OIDCClientSecretRequest {
	if in == nil {
		return nil
	}
	out := new(OIDCClientSecretRequest)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *OIDCClientSecretRequest) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *OIDCClientSecretRequestList) DeepCopyInto(out *OIDCClientSecretRequestList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]OIDCClientSecretRequest, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new OIDCClientSecretRequestList.
func (in *OIDCClientSecretRequestList) DeepCopy() *OIDCClientSecretRequestList {
	if in == nil {
		return nil
	}
	out := new(OIDCClientSecretRequestList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *OIDCClientSecretRequestList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *OIDCClientSecretRequestSpec) DeepCopyInto(out *OIDCClientSecretRequestSpec) {
	*out = *in
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new OIDCClientSecretRequestSpec.
func (in *OIDCClientSecretRequestSpec) DeepCopy() *OIDCClientSecretRequestSpec {
	if in == nil {
		return nil
	}
	out := new(OIDCClientSecretRequestSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *OIDCClientSecretRequestStatus) DeepCopyInto(out *OIDCClientSecretRequestStatus) {
	*out = *in
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new OIDCClientSecretRequestStatus.
func (in *OIDCClientSecretRequestStatus) DeepCopy() *OIDCClientSecretRequestStatus {
	if in == nil {
		return nil
	}
	out := new(OIDCClientSecretRequestStatus)
	in.DeepCopyInto(out)
	return out
}
