package awstest

// The ledger double carries more behaviour than any other in this package, and the tests
// that depend on it depend on it being strict.
//
// A store refuses a forbidden move by making the state it read a condition of the write.
// If this double ignored that condition, every one of those refusals would appear to
// succeed and the tests asserting them would pass for exactly the wrong reason. So the
// cases below are mostly about the double saying no: a condition that does not hold, an
// expression it cannot evaluate, a record that is not there.

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const ledgerTable = "ShareRecords"

func str(value string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: value}
}

// seedLedgerItem writes one record straight into the table.
func seedLedgerItem(table *DynamoDB, id string, attrs map[string]string) {
	item := map[string]types.AttributeValue{"shareId": str(id)}
	for name, value := range attrs {
		item[name] = str(value)
	}
	table.Items[id] = item
}

// stateUpdate builds the shape a store writes when it moves a record between states: the
// new state assigned, the state it read made a condition of the write.
func stateUpdate(id, to, expected string) *dynamodb.UpdateItemInput {
	return &dynamodb.UpdateItemInput{
		TableName:                 aws.String(ledgerTable),
		Key:                       map[string]types.AttributeValue{"shareId": str(id)},
		UpdateExpression:          aws.String("SET #state = :state"),
		ConditionExpression:       aws.String("#state = :expectedState"),
		ExpressionAttributeNames:  map[string]string{"#state": "state"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":state": str(to), ":expectedState": str(expected)},
		ReturnValues:              types.ReturnValueAllNew,
	}
}

func TestLedgerAppliesAnUpdateWhoseConditionHolds(t *testing.T) {
	table := NewLedger()
	seedLedgerItem(table, "one", map[string]string{"state": "staged"})

	out, err := table.UpdateItem(context.Background(), stateUpdate("one", "released", "staged"))
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	if got := out.Attributes["state"].(*types.AttributeValueMemberS).Value; got != "released" {
		t.Errorf("returned state = %q, want released", got)
	}
	if got := table.Items["one"]["state"].(*types.AttributeValueMemberS).Value; got != "released" {
		t.Errorf("stored state = %q, want released", got)
	}
}

// TestLedgerRefusesAnUpdateWhoseConditionDoesNotHold is the case the refusal tests rest
// on. The failure has to be the one a store watches for, and the record has to be left
// exactly as it was: a double that reported the refusal but wrote anyway would let a
// forbidden move through unnoticed.
func TestLedgerRefusesAnUpdateWhoseConditionDoesNotHold(t *testing.T) {
	table := NewLedger()
	seedLedgerItem(table, "one", map[string]string{"state": "revoked"})

	_, err := table.UpdateItem(context.Background(), stateUpdate("one", "released", "staged"))

	var refused *types.ConditionalCheckFailedException
	if !errors.As(err, &refused) {
		t.Fatalf("expected the condition to be refused the way the table refuses it, got %v", err)
	}
	if got := table.Items["one"]["state"].(*types.AttributeValueMemberS).Value; got != "revoked" {
		t.Errorf("stored state = %q; a refused write must change nothing", got)
	}
}

func TestLedgerRefusesAnUpdateToARecordThatIsNotThere(t *testing.T) {
	table := NewLedger()

	_, err := table.UpdateItem(context.Background(), stateUpdate("missing", "released", "staged"))

	var refused *types.ConditionalCheckFailedException
	if !errors.As(err, &refused) {
		t.Fatalf("expected an absent record to refuse the write, got %v", err)
	}
	if len(table.Items) != 0 {
		t.Error("a refused write created a record")
	}
}

// TestLedgerAppliesRemovals covers the other half of an update expression. Clearing an
// attribute is how a record stops saying two things at once -- a release returned to being
// grantable keeps no revocation timestamp -- so a removal silently skipped would leave a
// record that contradicts itself.
func TestLedgerAppliesRemovals(t *testing.T) {
	table := NewLedger()
	seedLedgerItem(table, "one", map[string]string{"state": "revoked", "revokedAt": "2026-07-10T04:00:00Z"})

	_, err := table.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName:                 aws.String(ledgerTable),
		Key:                       map[string]types.AttributeValue{"shareId": str("one")},
		UpdateExpression:          aws.String("SET #state = :state REMOVE #revokedAt"),
		ExpressionAttributeNames:  map[string]string{"#state": "state", "#revokedAt": "revokedAt"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":state": str("staged")},
	})
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	if _, present := table.Items["one"]["revokedAt"]; present {
		t.Error("the removal left the attribute in place")
	}
	if got := table.Items["one"]["state"].(*types.AttributeValueMemberS).Value; got != "staged" {
		t.Errorf("stored state = %q, want staged", got)
	}
}

// TestLedgerRejectsAnExpressionItCannotEvaluate checks the double fails loudly rather than
// quietly doing nothing. An expression skipped in silence is a test that passes without
// having exercised anything.
func TestLedgerRejectsAnExpressionItCannotEvaluate(t *testing.T) {
	table := NewLedger()
	seedLedgerItem(table, "one", map[string]string{"state": "staged"})

	_, err := table.UpdateItem(context.Background(), &dynamodb.UpdateItemInput{
		TableName:                aws.String(ledgerTable),
		Key:                      map[string]types.AttributeValue{"shareId": str("one")},
		UpdateExpression:         aws.String("ADD #count :one"),
		ExpressionAttributeNames: map[string]string{"#count": "count"},
	})
	if err == nil {
		t.Fatal("expected an expression this table cannot apply to fail rather than be ignored")
	}

	var refused *types.ConditionalCheckFailedException
	if errors.As(err, &refused) {
		t.Error("an expression it cannot apply is a fault in the test, not a refused condition")
	}
}

// TestLedgerQueryFiltersByKeyAndMechanism covers the read both stores make: an index keyed
// on one attribute, narrowed to the records of one mechanism. Getting the filter wrong
// would hand one store the other's records, which it cannot unmarshal.
func TestLedgerQueryFiltersByKeyAndMechanism(t *testing.T) {
	table := NewLedger()
	seedLedgerItem(table, "release", map[string]string{
		"researcherAccountId": "123456789012", "mechanism": "s3", "createdAt": "2026-07-10T04:00:00Z",
	})
	seedLedgerItem(table, "share", map[string]string{
		"researcherAccountId": "123456789012", "mechanism": "htsget", "createdAt": "2026-07-11T04:00:00Z",
	})
	seedLedgerItem(table, "other-account", map[string]string{
		"researcherAccountId": "999999999999", "mechanism": "s3", "createdAt": "2026-07-12T04:00:00Z",
	})

	out, err := table.Query(context.Background(), &dynamodb.QueryInput{
		TableName:                aws.String(ledgerTable),
		IndexName:                aws.String("GSI2"),
		KeyConditionExpression:   aws.String("researcherAccountId = :accountId"),
		FilterExpression:         aws.String("#mechanism = :releaseMechanism"),
		ExpressionAttributeNames: map[string]string{"#mechanism": "mechanism"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":accountId": str("123456789012"), ":releaseMechanism": str("s3"),
		},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("expected one record, got %d", len(out.Items))
	}
	if got := out.Items[0]["shareId"].(*types.AttributeValueMemberS).Value; got != "release" {
		t.Errorf("query returned %q, want the release of that account", got)
	}
}

// TestLedgerScanAdmitsRecordsWrittenBeforeThereWereTwoMechanisms covers the disjunction the
// older mechanism reads with: a record carrying no mechanism attribute is one of its own,
// written before the table held both kinds.
func TestLedgerScanAdmitsRecordsWrittenBeforeThereWereTwoMechanisms(t *testing.T) {
	table := NewLedger()
	seedLedgerItem(table, "unmarked", map[string]string{"createdAt": "2026-07-10T04:00:00Z"})
	seedLedgerItem(table, "htsget", map[string]string{"mechanism": "htsget", "createdAt": "2026-07-11T04:00:00Z"})
	seedLedgerItem(table, "release", map[string]string{"mechanism": "s3", "createdAt": "2026-07-12T04:00:00Z"})

	out, err := table.Scan(context.Background(), &dynamodb.ScanInput{
		TableName:                 aws.String(ledgerTable),
		FilterExpression:          aws.String("attribute_not_exists(#mechanism) OR #mechanism = :htsgetMechanism"),
		ExpressionAttributeNames:  map[string]string{"#mechanism": "mechanism"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":htsgetMechanism": str("htsget")},
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("expected the unmarked record and the htsget one, got %d", len(out.Items))
	}
	// Newest first, which is the order every listing wants and a real scan does not
	// promise.
	if got := out.Items[0]["shareId"].(*types.AttributeValueMemberS).Value; got != "htsget" {
		t.Errorf("scan returned %q first, want the newest record", got)
	}
}

// TestLedgerQueryOrdersByCreation checks the sort direction a query asks for, since the
// stores rely on newest first to pick the release that matters.
func TestLedgerQueryOrdersByCreation(t *testing.T) {
	table := NewLedger()
	for id, createdAt := range map[string]string{
		"older":  "2026-07-10T04:00:00Z",
		"newer":  "2026-07-12T04:00:00Z",
		"middle": "2026-07-11T04:00:00Z",
	} {
		seedLedgerItem(table, id, map[string]string{"datasetUri": "s3://source/cohort-a/", "createdAt": createdAt})
	}

	query := func(ascending bool) []string {
		out, err := table.Query(context.Background(), &dynamodb.QueryInput{
			TableName:                 aws.String(ledgerTable),
			IndexName:                 aws.String("GSI3"),
			KeyConditionExpression:    aws.String("#datasetUri = :datasetUri"),
			ExpressionAttributeNames:  map[string]string{"#datasetUri": "datasetUri"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":datasetUri": str("s3://source/cohort-a/")},
			ScanIndexForward:          aws.Bool(ascending),
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		ids := make([]string, 0, len(out.Items))
		for _, item := range out.Items {
			ids = append(ids, item["shareId"].(*types.AttributeValueMemberS).Value)
		}
		return ids
	}

	descending := query(false)
	for i, want := range []string{"newer", "middle", "older"} {
		if descending[i] != want {
			t.Errorf("descending[%d] = %q, want %q", i, descending[i], want)
		}
	}
	ascending := query(true)
	for i, want := range []string{"older", "middle", "newer"} {
		if ascending[i] != want {
			t.Errorf("ascending[%d] = %q, want %q", i, ascending[i], want)
		}
	}
}
