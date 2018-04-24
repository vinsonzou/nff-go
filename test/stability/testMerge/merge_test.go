// Copyright 2017 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"testing"
)

func TestInit(t *testing.T) {
	if err := initDPDK(); err != nil {
        t.Fatalf("fail to initialize with error: %+v\n", err)
    }
}

func TestMerge(t *testing.T) {
	scenarios := []pipelineScenario {getGet, inCopy, copyCopy, segmSegm, segmGet, segmCopy, inSegm, manyFlows}
	gens := []getScenario {fastGenerate, generate}
	addSegmVariants := []bool {true, false}
	for _,scenario := range scenarios {
		for _,gen := range gens {
			for _,addSegm := range addSegmVariants {
				t.Logf("scenario: %d, generator type: %d, add segment after merge: %t", scenario, gen, addSegm)
				err := executeTest("", "", gotest, scenario, gen, addSegm)
				resetState()	
				if err != nil {
					t.Logf("fail: %+v\n", err)
					t.Fail()
				}
			}
		}
	}
}