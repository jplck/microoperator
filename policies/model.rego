package microorchestrator

default action := "deny"

action := "allow" if {
	input.spec_version == "acs/v0.1"
	input.intervention_point == "pre_model_call"
	input.source.verified == true
	input.request.model == "allowed"
}
