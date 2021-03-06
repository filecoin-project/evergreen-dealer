package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/filecoin-project/evergreen-dealer/common"
	"github.com/filecoin-project/evergreen-dealer/webapi/types"
	"github.com/jackc/pgx/v4"
	"github.com/labstack/echo/v4"
)

func apiListEligible(c echo.Context) error {
	ctx := c.Request().Context()
	spID := c.Response().Header().Get("X-FIL-SPID")
	spSize, err := strconv.ParseUint(c.Response().Header().Get("X-FIL-SPSIZE"), 10, 64)
	if err != nil {
		return err
	}

	lim := uint64(128)
	limStr := c.QueryParams().Get("limit")
	if limStr != "" {
		var err error
		lim, err = strconv.ParseUint(limStr, 10, 64)
		if err != nil {
			return retFail(c, nil, "provided limit '%s' is not a valid integer", limStr)
		}
	}

	internalReason, err := spIneligibleReason(ctx, spID)
	if err != nil {
		return err
	} else if internalReason != "" {
		return retFail(c, internalReason, ineligibleSpMsg(spID))
	}

	// only query them in the `anywhere` case
	var spOrgID, spCity, spCountry, spContinent string
	var maxPerOrg, maxPerCity, maxPerCountry, maxPerContinent, programMax int64

	commonInfoFooter := strings.Join([]string{
		`Once you have selected a Piece CID you would like to renew, and are reasonably confident`,
		`you can obtain the data for it - request a deal from the system by invoking the API as`,
		"shown in the corresponding `sample_request_cmd`. You will then receive a deal with 10 minutes,",
		"and can proceed to `lotus-miner storage-deals import-data ...` the corresponding car file.",
		``,
		`In order to see what proposals you have currently pending, you can invoke:`,
		" " + urlAuthedFor(c, spID, "/pending_proposals"),
	}, "\n")

	var rows pgx.Rows
	var info string
	if c.Request().URL.Path == "/eligible_pieces/sp_local" {

		info = strings.Join([]string{
			fmt.Sprintf(`List of qualifying Piece CIDs currently available within SPS %s itself`, spID),
			``,
			`This list is ordered by most recently expiring/expired first, and reflects all pieces of data`,
			`that are still present within your own SP. It is recommended you perform these renewals first,`,
			`as data for them is readily obtainable.`,
			``,
			commonInfoFooter,
		}, "\n")

		rows, err = common.Db.Query(
			ctx,
			`
			WITH
				providers_in_org AS (
					SELECT provider_id FROM providers WHERE org_id IN ( SELECT city FROM providers WHERE provider_id = $1 )
				)
			SELECT
					d.dataset_slug,
					d.padded_size,
					d.piece_cid,
					d.deal_id,
					d.original_payload_cid,
					d.normalized_payload_cid,
					d.provider_id,
					d.is_filplus,
					d.end_time,
					NULL,
					NULL
				FROM deallist_eligible d
			WHERE

				d.provider_id = $1

					AND

				d.end_time < expiration_cutoff()

					AND

				-- the limit of active nonexpiring + in-fight deals within my org is not violated
				max_per_org() > (
					(
						SELECT COUNT(*)
							FROM published_deals pd
							JOIN clients c USING ( client_id )
							JOIN providers_in_org USING ( provider_id )
						WHERE
							pd.piece_cid = d.piece_cid
								AND
							c.is_affiliated
								AND
							pd.status = 'active'
								AND
							NOT COALESCE( (pd.meta->'inactive')::BOOL, false )
								AND
							pd.end_time > expiration_cutoff()
					)
						+
					(
						SELECT COUNT(*)
							FROM proposals pr
							JOIN providers_in_org USING ( provider_id )
						WHERE
							pr.piece_cid = d.piece_cid
								AND
							pr.proposal_failstamp = 0
								AND
							pr.activated_deal_id IS NULL
					)
				)
			`,
			spID,
		)
	} else {
		info = strings.Join([]string{
			`List of qualifying Piece CIDs together with their availability from various sources.`,
			``,
			`In order to satisfy a FilPlus deal from the evergreen engine, all you need to do is obtain the `,
			`corresponding .car file (usually by retrieving it from one of the sources within this list).`,
			``,
			commonInfoFooter,
		}, "\n")

		err = common.Db.QueryRow(
			ctx,
			`SELECT
					org_id,
					city,
					country,
					continent,
					max_per_org(),
					max_per_city(),
					max_per_country(),
					max_per_continent(),
					max_program_replicas()
				FROM providers
			WHERE provider_id = $1`,
			spID,
		).Scan(&spOrgID, &spCity, &spCountry, &spContinent, &maxPerOrg, &maxPerCity, &maxPerCountry, &maxPerContinent, &programMax)
		if err != nil {
			return err
		}

		rows, err = common.Db.Query(
			ctx,
			`
			SELECT
					d.dataset_slug,
					d.padded_size,
					d.piece_cid,
					d.deal_id,
					d.original_payload_cid,
					d.normalized_payload_cid,
					d.provider_id,
					d.is_filplus,
					d.end_time,
					rc.active AS counts_replicas,
					rc.pending AS counts_pending
				FROM deallist_eligible d
				JOIN replica_counts rc USING ( piece_cid )
			WHERE

				d.padded_size <= $1

					AND

				-- exclude my own in-flight proposals / actives
				NOT EXISTS (
					SELECT 42
						FROM proposals pr
					WHERE
						pr.piece_cid = d.piece_cid
							AND
						pr.proposal_failstamp = 0
							AND
						pr.activated_deal_id IS NULL
							AND
						pr.provider_id = $2
				)

					AND

				NOT EXISTS (
					SELECT 42
						FROM published_deals pd
					WHERE
						pd.piece_cid = d.piece_cid
							AND
						pd.status != 'terminated'
							AND
						NOT COALESCE( (pd.meta->'inactive')::BOOL, false )
							AND
						pd.provider_id = $2
				)
			`,
			spSize,
			spID,
		)
	}
	if err != nil {
		return err
	}
	defer rows.Close()

	type pieceSpCombo struct {
		pcid string
		spid string
	}

	type aggCounts map[string]map[string]int64

	pieces := make(map[string]*types.Piece, 1024)
	seenPieceSpCombo := make(map[pieceSpCombo]int64, 32768)
	ineligiblePcids := make(map[string]struct{}, 2048)
	for rows.Next() {
		var s types.FilSource
		var p types.Piece
		var repCountsJSON, propCountsJSON *string

		if err = rows.Scan(&p.Dataset, &p.PaddedPieceSize, &p.PieceCid, &s.DealID, &s.OriginalPayloadCid, &s.NormalizedPayloadCid, &s.ProviderID, &s.IsFilplus, &s.DealExpiration, &repCountsJSON, &propCountsJSON); err != nil {
			return err
		}

		if prevDealID, seen := seenPieceSpCombo[pieceSpCombo{pcid: p.PieceCid, spid: s.ProviderID}]; seen {
			return fmt.Errorf("Unexpected double-deal for same sp/pcid: %d and %d", prevDealID, s.DealID)
		}
		seenPieceSpCombo[pieceSpCombo{pcid: p.PieceCid, spid: s.ProviderID}] = s.DealID

		if p.PaddedPieceSize > spSize {
			continue
		}
		if _, ineligible := ineligiblePcids[p.PieceCid]; ineligible {
			continue
		}

		if _, seen := pieces[p.PieceCid]; !seen {

			if repCountsJSON != nil {
				var active, proposed aggCounts
				if err := json.Unmarshal([]byte(*repCountsJSON), &active); err != nil {
					return err
				}
				if err := json.Unmarshal([]byte(*propCountsJSON), &proposed); err != nil {
					return err
				}

				if programMax <= active["total"]["total"]+proposed["total"]["total"] ||
					maxPerOrg <= active["org_id"][spOrgID]+proposed["org_id"][spOrgID] ||
					maxPerCity <= active["city"][spCity]+proposed["city"][spCity] ||
					maxPerCountry <= active["country"][spCountry]+proposed["country"][spCountry] ||
					maxPerContinent <= active["continent"][spContinent]+proposed["continent"][spContinent] {

					ineligiblePcids[p.PieceCid] = struct{}{}
					continue
				}
			}

			p.PayloadCids = append(p.PayloadCids, s.NormalizedPayloadCid)
			p.SampleRequestCmd = urlAuthedFor(c, spID, "/request_piece/"+p.PieceCid)
			pieces[p.PieceCid] = &p
		}

		s.PieceCid = p.PieceCid
		s.InitDerivedVals()
		pieces[p.PieceCid].Sources = append(pieces[p.PieceCid].Sources, &s)
	}

	ret := make(types.ResponsePiecesEligible, 0, 2048)
	for _, p := range pieces {
		sort.Slice(p.Sources, func(i, j int) bool {
			switch {

			case p.Sources[i].SrcType() != p.Sources[j].SrcType():
				return p.Sources[i].SrcType() < p.Sources[j].SrcType()

			case p.Sources[i].ExpiryUnixNano() != p.Sources[j].ExpiryUnixNano():
				return p.Sources[i].ExpiryUnixNano() > p.Sources[j].ExpiryUnixNano()

			default:
				return p.Sources[i].SysID() < p.Sources[j].SysID()
			}
		})
		ret = append(ret, p)
	}

	sort.Slice(ret, func(i, j int) bool {
		si, sj := ret[i].Sources[len(ret[i].Sources)-1], ret[j].Sources[len(ret[j].Sources)-1]

		switch {

		case si.ExpiryCoarse() != sj.ExpiryCoarse():
			return si.ExpiryCoarse() < sj.ExpiryCoarse()

		default:
			return ret[i].PieceCid < ret[j].PieceCid

		}
	})

	if uint64(len(ret)) > lim {
		info = strings.Join([]string{
			fmt.Sprintf(`NOTE: The complete list of %d entries has been TRUNCATED to the top %d.`, len(ret), lim),
			"You can use the 'limit' param in your API call to see the full (possibly very large) list:",
			" " + urlAuthedFor(c, spID, fmt.Sprintf("%s?limit=%d", c.Request().URL.Path, len(ret))),
			"",
			info,
		}, "\n")
		ret = ret[:lim]
	}

	return retPayloadAnnotated(c, http.StatusOK, ret, info)
}
