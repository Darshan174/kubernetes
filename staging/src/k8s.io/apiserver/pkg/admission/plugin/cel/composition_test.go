/*
Copyright 2023 The Kubernetes Authors.

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

package cel

import (
	"context"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"

	"k8s.io/apiserver/pkg/admission"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

type testVariable struct {
	name       string
	expression string
}

func (t *testVariable) GetExpression() string {
	return t.expression
}

func (t *testVariable) ReturnTypes() []*cel.Type {
	return []*cel.Type{cel.AnyType}
}

func (t *testVariable) GetName() string {
	return t.name
}

func TestCompositedPolicies(t *testing.T) {
	cases := []struct {
		name                 string
		variables            []NamedExpressionAccessor
		expression           string
		attributes           admission.Attributes
		expectedResult       any
		expectErr            bool
		expectedErrorMessage string
		runtimeCostBudget    int64
	}{
		{
			name: "simple",
			variables: []NamedExpressionAccessor{
				&testVariable{
					name:       "name",
					expression: "object.metadata.name",
				},
			},
			attributes:     endpointCreateAttributes(),
			expression:     "variables.name == 'endpoints1'",
			expectedResult: true,
		},
		{
			name: "delayed compile error",
			variables: []NamedExpressionAccessor{
				&testVariable{
					name:       "name",
					expression: "1 == '1'", // won't compile
				},
			},
			attributes:           endpointCreateAttributes(),
			expression:           "variables.name == 'endpoints1'",
			expectErr:            true,
			expectedErrorMessage: `composited variable "name" fails to compile:`,
		},
		{
			name: "delayed eval error",
			variables: []NamedExpressionAccessor{
				&testVariable{
					name:       "name",
					expression: "object.spec.subsets[114514].addresses.size()", // array index out of bound
				},
			},
			attributes:           endpointCreateAttributes(),
			expression:           "variables.name == 'endpoints1'",
			expectErr:            true,
			expectedErrorMessage: `composited variable "name" fails to evaluate:`,
		},
		{
			name: "out of budget during lazy evaluation",
			variables: []NamedExpressionAccessor{
				&testVariable{
					name:       "name",
					expression: "object.metadata.name", // cost = 3
				},
			},
			attributes:           endpointCreateAttributes(),
			expression:           "variables.name == 'endpoints1'", // cost = 3
			expectedResult:       true,
			runtimeCostBudget:    4, // enough for main variable but not for entire expression
			expectErr:            true,
			expectedErrorMessage: "running out of cost budget",
		},
		{
			name: "lazy evaluation, budget counts only once",
			variables: []NamedExpressionAccessor{
				&testVariable{
					name:       "name",
					expression: "object.metadata.name", // cost = 3
				},
			},
			attributes:        endpointCreateAttributes(),
			expression:        "variables.name == 'endpoints1' && variables.name == 'endpoints1' ", // cost = 7
			expectedResult:    true,
			runtimeCostBudget: 10, // enough for one lazy evaluation but not two, should pass
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiler, err := NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
			if err != nil {
				t.Fatal(err)
			}
			compiler.CompileAndStoreVariables(tc.variables, OptionalVariableDeclarations{HasParams: false, HasAuthorizer: false}, environment.NewExpressions)
			validations := []ExpressionAccessor{&condition{Expression: tc.expression}}
			f := compiler.Compile(validations, OptionalVariableDeclarations{HasParams: false, HasAuthorizer: false}, environment.NewExpressions)
			versionedAttr, err := admission.NewVersionedAttributes(tc.attributes, tc.attributes.GetKind(), newObjectInterfacesForTest())
			if err != nil {
				t.Fatal(err)
			}
			optionalVars := OptionalVariableBindings{}
			costBudget := tc.runtimeCostBudget
			if costBudget == 0 {
				costBudget = celconfig.RuntimeCELCostBudget
			}
			result, _, err := f.ForInput(context.Background(), versionedAttr, CreateAdmissionRequest(versionedAttr.Attributes), optionalVars, costBudget)
			if !tc.expectErr && err != nil {
				t.Fatalf("failed evaluation: %v", err)
			}
			if !tc.expectErr && len(result) == 0 {
				t.Fatal("unexpected empty result")
			}
			if err == nil {
				err = result[0].Error
			}
			if tc.expectErr {
				if err == nil {
					t.Fatal("unexpected no error")
				}
				if !strings.Contains(err.Error(), tc.expectedErrorMessage) {
					t.Errorf("expected error to contain %q but got %s", tc.expectedErrorMessage, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("failed validation: %v", result[0].Error)
			}
			if tc.expectedResult != result[0].EvalResult.Value() {
				t.Errorf("wrong result: expected %v but got %v", tc.expectedResult, result)
			}

		})
	}
}
