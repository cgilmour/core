// Copyright (c) 2016 Pani Networks
// All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package tenant

import (
	"github.com/go-check/check"
	"log"
	"testing"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) {
	check.TestingT(t)
}

type MySuite struct {
}

var _ = check.Suite(&MySuite{})

func (s *MySuite) TestStore(c *check.C) {
	var err error
	var t Tenant
	var seg Segment

	store := tenantStore{}
	store.ServiceStore = &store

	storeConfig := make(map[string]interface{})
	storeConfig["type"] = "sqlite3"
	storeConfig["database"] = "/var/tmp/tenantTest.sqlite3"
	err = store.SetConfig(storeConfig)
	c.Assert(err, check.IsNil)
	err = store.CreateSchema(true)
	c.Assert(err, check.IsNil)

	// Should be OK
	t = Tenant{Name: "name1"}
	err = store.addTenant(&t)
	c.Assert(err, check.IsNil)

	tenID1 := t.ID
	log.Printf("Created tenant %+v", t)

	// Error: duplicate name
	t = Tenant{Name: "name1"}
	err = store.addTenant(&t)
	c.Assert(err, check.NotNil, check.Commentf("Expected error"))
	log.Printf("Expected error %T %+v", err, err)

	// OK: external ID disambiguates.
	t = Tenant{Name: "name1", ExternalID: "extid1"}
	err = store.addTenant(&t)
	c.Assert(err, check.IsNil)

	tenID2 := t.ID
	log.Printf("Created tenant %+v", t)

	// Error: duplicate
	t = Tenant{Name: "name1", ExternalID: "extid1"}
	err = store.addTenant(&t)
	c.Assert(err, check.NotNil, check.Commentf("Expected error"))
	log.Printf("Expected error %T %+v", err, err)

	// OK
	t = Tenant{Name: "xxx", ExternalID: "extid1"}
	err = store.addTenant(&t)
	c.Assert(err, check.IsNil)

	// OK
	t = Tenant{ExternalID: "extid2"}
	err = store.addTenant(&t)
	c.Assert(err, check.IsNil)
	log.Printf("Created tenant %+v", t)

	// Duplicate
	t = Tenant{ExternalID: "extid2"}
	err = store.addTenant(&t)
	c.Assert(err, check.NotNil, check.Commentf("Expected error"))
	log.Printf("Expected error %T %+v", err, err)

	// OK
	seg = Segment{Name: "seg1"}
	err = store.addSegment(tenID1, &seg)
	c.Assert(err, check.IsNil)
	log.Printf("Created segment %+v", seg)

	// Duplicate
	seg = Segment{Name: "seg1"}
	err = store.addSegment(tenID1, &seg)
	c.Assert(err, check.NotNil, check.Commentf("Expected error"))
	log.Printf("Expected error %T %+v", err, err)

	// OK
	seg = Segment{Name: "seg1", ExternalID: "segextid1"}
	err = store.addSegment(tenID1, &seg)
	c.Assert(err, check.IsNil)
	log.Printf("Created segment %+v", seg)

	// Duplicate
	seg = Segment{Name: "seg1", ExternalID: "segextid1"}
	err = store.addSegment(tenID1, &seg)
	c.Assert(err, check.NotNil, check.Commentf("Expected error"))
	log.Printf("Expected error %T %+v", err, err)

	// OK - different tenant
	seg = Segment{Name: "seg1"}
	err = store.addSegment(tenID2, &seg)
	c.Assert(err, check.IsNil)
	log.Printf("Created segment %+v", seg)

	// OK
	seg = Segment{ExternalID: "segextid2"}
	err = store.addSegment(tenID1, &seg)
	c.Assert(err, check.IsNil)
	log.Printf("Created segment %+v", seg)

	// Duplicate
	seg = Segment{ExternalID: "segextid2"}
	err = store.addSegment(tenID1, &seg)
	c.Assert(err, check.NotNil, check.Commentf("Expected error"))
	log.Printf("Expected error %T %+v", err, err)

	c.Assert("", check.Equals, "")
}
