package v1

func (in *FunctionSpec) DeepCopyInto(out *FunctionSpec) {
	*out = *in
	in.Runtime.DeepCopyInto(&out.Runtime)
}

func (in *FunctionSpec) DeepCopy() *FunctionSpec {
	if in == nil {
		return nil
	}
	out := new(FunctionSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RuntimeSpec) DeepCopyInto(out *RuntimeSpec) {
	*out = *in
	if in.Replicas != nil {
		out.Replicas = new(int32)
		*out.Replicas = *in.Replicas
	}
	in.Resources.DeepCopyInto(&out.Resources)
}

func (in *RuntimeSpec) DeepCopy() *RuntimeSpec {
	if in == nil {
		return nil
	}
	out := new(RuntimeSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *FunctionStatus) DeepCopyInto(out *FunctionStatus) {
	*out = *in
	if in.Functions != nil {
		out.Functions = append([]string(nil), in.Functions...)
	}
}

func (in *FunctionStatus) DeepCopy() *FunctionStatus {
	if in == nil {
		return nil
	}
	out := new(FunctionStatus)
	in.DeepCopyInto(out)
	return out
}
