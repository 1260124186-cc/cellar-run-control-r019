package workflowcheck

import (
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func vesselRunLifecycle(api *client) error {
	formula, err := ensureApprovedFormula(api, "Lifecycle Formula")
	if err != nil {
		return err
	}
	vesselResponse, err := api.call("POST", "/v1/vessels", map[string]any{
		"code":            "VSL-101",
		"capacity_liters": 240.0,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(vesselResponse, 201, "create vessel"); err != nil {
		return err
	}
	var vessel vesselView
	if err := vesselResponse.decode(&vessel); err != nil {
		return err
	}

	firstResponse, err := api.call("POST", "/v1/runs",
		createRunPayload("RUN-LIFECYCLE-A", formula.ID), nil)
	if err != nil {
		return err
	}
	if err := requireStatus(firstResponse, 201, "create first run"); err != nil {
		return err
	}
	var first runView
	if err := firstResponse.decode(&first); err != nil {
		return err
	}

	secondResponse, err := api.call("POST", "/v1/runs",
		createRunPayload("RUN-LIFECYCLE-B", formula.ID), nil)
	if err != nil {
		return err
	}
	if err := requireStatus(secondResponse, 201, "create second run"); err != nil {
		return err
	}
	var second runView
	if err := secondResponse.decode(&second); err != nil {
		return err
	}

	reservedResponse, err := api.call("POST", "/v1/runs/"+first.ID+"/reserve",
		map[string]any{"vessel_id": vessel.ID}, ptrInt64(1))
	if err != nil {
		return err
	}
	if err := requireStatus(reservedResponse, 200, "reserve vessel"); err != nil {
		return err
	}
	var pair pairView
	if err := reservedResponse.decode(&pair); err != nil {
		return err
	}
	if err := requireRunState(pair.Run, domain.RunReserved); err != nil {
		return err
	}
	if err := requireVesselState(pair.Vessel, domain.VesselReserved); err != nil {
		return err
	}

	conflictResponse, err := api.call("POST", "/v1/runs/"+second.ID+"/reserve",
		map[string]any{"vessel_id": vessel.ID}, ptrInt64(1))
	if err != nil {
		return err
	}
	if err := requireCode(conflictResponse, 409, string(domain.CodeVesselUnavailable),
		"reserve occupied vessel"); err != nil {
		return err
	}

	startResponse, err := api.call("POST", "/v1/runs/"+first.ID+"/start",
		nil, ptrInt64(2))
	if err != nil {
		return err
	}
	if err := requireStatus(startResponse, 200, "start run"); err != nil {
		return err
	}
	if err := startResponse.decode(&pair); err != nil {
		return err
	}
	if err := requireRunState(pair.Run, domain.RunFermenting); err != nil {
		return err
	}
	if err := requireVesselState(pair.Vessel, domain.VesselInUse); err != nil {
		return err
	}
	if pair.Run.VesselID == nil || *pair.Run.VesselID != vessel.ID {
		return errInvalidWorkflow("run did not retain its reserved vessel")
	}

	readResponse, err := api.call("GET", "/v1/runs/"+first.ID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(readResponse, 200, "read started run"); err != nil {
		return err
	}
	if err := readResponse.decode(&first); err != nil {
		return err
	}
	if err := requireRunState(first, domain.RunFermenting); err != nil {
		return err
	}

	// Aborting a started run seals it and sends the in-use vessel to cleaning.
	abortResponse, err := api.call("POST", "/v1/runs/"+first.ID+"/abort",
		nil, ptrInt64(pair.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(abortResponse, 200, "abort started run"); err != nil {
		return err
	}
	var aborted pairView
	if err := abortResponse.decode(&aborted); err != nil {
		return err
	}
	if err := requireRunState(aborted.Run, domain.RunAborted); err != nil {
		return err
	}
	if err := requireVesselState(aborted.Vessel, domain.VesselCleaning); err != nil {
		return err
	}
	if aborted.Run.EndedAt == nil {
		return errInvalidWorkflow("aborted run has no end time")
	}
	sealedRunVersion := aborted.Run.Version
	sealedVesselVersion := aborted.Vessel.Version
	sealedRunUpdatedAt := aborted.Run.UpdatedAt
	sealedVesselUpdatedAt := aborted.Vessel.UpdatedAt

	sealedObservation, err := api.call("POST", "/v1/runs/"+first.ID+"/observations",
		map[string]any{
			"sequence":      1,
			"gravity":       1.04,
			"temperature_c": 20,
			"observed_at":   time.Now().UTC().Add(time.Minute),
			"note":          "late reading after abort",
		}, ptrInt64(sealedRunVersion))
	if err != nil {
		return err
	}
	if err := requireCode(sealedObservation, 409, string(domain.CodeRunState),
		"append observation to aborted run"); err != nil {
		return err
	}

	repeatAbort, err := api.call("POST", "/v1/runs/"+first.ID+"/abort",
		nil, ptrInt64(sealedRunVersion))
	if err != nil {
		return err
	}
	if err := requireCode(repeatAbort, 409, string(domain.CodeRunState),
		"abort run twice"); err != nil {
		return err
	}

	lateComplete, err := api.call("POST", "/v1/runs/"+first.ID+"/complete",
		nil, ptrInt64(sealedRunVersion))
	if err != nil {
		return err
	}
	if err := requireCode(lateComplete, 409, string(domain.CodeRunState),
		"complete an aborted run"); err != nil {
		return err
	}

	readAborted, err := api.call("GET", "/v1/runs/"+first.ID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(readAborted, 200, "read aborted run"); err != nil {
		return err
	}
	var abortedRead runView
	if err := readAborted.decode(&abortedRead); err != nil {
		return err
	}
	if err := requireRunState(abortedRead, domain.RunAborted); err != nil {
		return err
	}
	if abortedRead.Version != sealedRunVersion {
		return errInvalidWorkflow("rejected write changed the aborted run version")
	}
	if len(abortedRead.Observations) != 0 {
		return errInvalidWorkflow("aborted run accepted a late observation")
	}
	if !abortedRead.UpdatedAt.Equal(sealedRunUpdatedAt) {
		return errInvalidWorkflow("rejected write changed the aborted run update time")
	}

	readVessel, err := api.call("GET", "/v1/vessels/"+vessel.ID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(readVessel, 200, "read aborted run vessel"); err != nil {
		return err
	}
	var abortedVessel vesselView
	if err := readVessel.decode(&abortedVessel); err != nil {
		return err
	}
	if err := requireVesselState(abortedVessel, domain.VesselCleaning); err != nil {
		return err
	}
	if abortedVessel.Version != sealedVesselVersion {
		return errInvalidWorkflow("rejected abort changed the vessel version")
	}
	if !abortedVessel.UpdatedAt.Equal(sealedVesselUpdatedAt) {
		return errInvalidWorkflow("rejected abort changed the vessel update time")
	}

	// Aborting before fermentation releases the reserved vessel and is sealed too.
	earlyVessel, err := createCheckVessel(api, "VSL-103", 240)
	if err != nil {
		return err
	}
	earlyRunResponse, err := api.call("POST", "/v1/runs",
		createRunPayload("RUN-LIFECYCLE-C", formula.ID), nil)
	if err != nil {
		return err
	}
	if err := requireStatus(earlyRunResponse, 201, "create third run"); err != nil {
		return err
	}
	var third runView
	if err := earlyRunResponse.decode(&third); err != nil {
		return err
	}
	earlyReserve, err := api.call("POST", "/v1/runs/"+third.ID+"/reserve",
		map[string]any{"vessel_id": earlyVessel.ID}, ptrInt64(third.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(earlyReserve, 200, "reserve vessel for third run"); err != nil {
		return err
	}
	var earlyPair pairView
	if err := earlyReserve.decode(&earlyPair); err != nil {
		return err
	}
	earlyAbort, err := api.call("POST", "/v1/runs/"+third.ID+"/abort",
		nil, ptrInt64(earlyPair.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(earlyAbort, 200, "abort reserved run"); err != nil {
		return err
	}
	var earlyAborted pairView
	if err := earlyAbort.decode(&earlyAborted); err != nil {
		return err
	}
	if err := requireRunState(earlyAborted.Run, domain.RunAborted); err != nil {
		return err
	}
	if err := requireVesselState(earlyAborted.Vessel, domain.VesselAvailable); err != nil {
		return err
	}
	if earlyAborted.Vessel.ActiveRunID != nil {
		return errInvalidWorkflow("pre-start abort did not release the vessel")
	}
	earlyObservation, err := api.call("POST", "/v1/runs/"+third.ID+"/observations",
		map[string]any{
			"sequence":      1,
			"gravity":       1.04,
			"temperature_c": 20,
			"observed_at":   time.Now().UTC().Add(2 * time.Minute),
			"note":          "late reading after early abort",
		}, ptrInt64(earlyAborted.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(earlyObservation, 409, string(domain.CodeRunState),
		"append observation to pre-start aborted run"); err != nil {
		return err
	}
	return nil
}

type workflowError string

func (e workflowError) Error() string {
	return string(e)
}

func errInvalidWorkflow(message string) error {
	return workflowError(message)
}
