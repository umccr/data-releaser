package awstest

// The double in this file stands in for the shared record table rather than for DynamoDB
// in general.
//
// The plain DynamoDB double keeps items and little else: its update applies two named
// attributes and its query answers one lookup. That is enough for a caller that writes a
// record and reads it back, and not nearly enough for one whose correctness is in the
// expressions themselves. A release moves through its lifecycle by conditional update,
// and every refusal it depends on is a condition the table evaluates -- so a double that
// ignores conditions reports every forbidden move as having succeeded, which is the one
// answer a test of a refusal must never be given.
//
// So this one reads the expressions. It evaluates the conditions before it writes,
// applies the assignments and removals it was given, filters a query by its key and by
// whichever mechanism the caller asked for, and orders results by creation time. Only the
// forms this project's stores actually write are understood; anything else fails the call
// rather than being quietly ignored, since an expression silently skipped is a test that
// passes for the wrong reason.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// createdAtAttribute is the sort key of both per-mechanism indexes, and so the order a
// listing comes back in.
const createdAtAttribute = "createdAt"

// NewLedger returns a double behaving like the shared record table: it honours the
// conditions of an update, applies the update's own expression, and answers the index
// queries and the scan that both mechanisms' stores make.
//
// Items are reachable through the embedded map as they are on the plain double, so a test
// can still seed a record in a state no legal move would produce.
func NewLedger() *DynamoDB {
	table := NewDynamoDB()
	table.UpdateItemFn = table.conditionalUpdate
	table.QueryFn = table.indexQuery
	table.ScanFn = table.filteredScan
	return table
}

// conditionalUpdate applies an update the way the table would. A condition that does not
// hold leaves the item exactly as it was and reports the failure a store watches for, so
// that a refusal is observed rather than assumed.
func (m *DynamoDB) conditionalUpdate(
	_ context.Context,
	params *dynamodb.UpdateItemInput,
) (*dynamodb.UpdateItemOutput, error) {
	id, ok := shareID(params.Key)
	if !ok {
		return nil, fmt.Errorf("update names no shareId in its key: %v", params.Key)
	}

	item, exists := m.Items[id]
	if !exists {
		return nil, &types.ConditionalCheckFailedException{}
	}

	holds, err := conditionHolds(item, aws.ToString(params.ConditionExpression), params.ExpressionAttributeNames, params.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}
	if !holds {
		return nil, &types.ConditionalCheckFailedException{}
	}

	// Replaced rather than mutated, so a snapshot a test took earlier stays as it was.
	updated := maps.Clone(item)
	if err := applyUpdate(updated, params); err != nil {
		return nil, err
	}
	m.Items[id] = updated

	if params.ReturnValues != types.ReturnValueAllNew {
		return &dynamodb.UpdateItemOutput{}, nil
	}
	return &dynamodb.UpdateItemOutput{Attributes: maps.Clone(updated)}, nil
}

// indexQuery answers a query over either of the table's secondary indexes: the key
// condition selects on one attribute, the filter narrows the result to one mechanism, and
// the sort direction orders by creation time.
func (m *DynamoDB) indexQuery(
	_ context.Context,
	params *dynamodb.QueryInput,
) (*dynamodb.QueryOutput, error) {
	name, want, err := keyCondition(aws.ToString(params.KeyConditionExpression), params.ExpressionAttributeNames, params.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	var items []map[string]types.AttributeValue
	for _, item := range m.Items {
		if !equalAttributes(item[name], want) {
			continue
		}
		matches, err := conditionHolds(item, aws.ToString(params.FilterExpression), params.ExpressionAttributeNames, params.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		if matches {
			items = append(items, item)
		}
	}

	sortByCreation(items, params.ScanIndexForward == nil || *params.ScanIndexForward)
	return &dynamodb.QueryOutput{Items: items}, nil
}

// filteredScan answers a scan of the whole table, narrowed by whichever mechanism the
// caller asked for. Results come back newest first, which is the order every listing
// wants and the order a real scan does not promise.
func (m *DynamoDB) filteredScan(
	_ context.Context,
	params *dynamodb.ScanInput,
) (*dynamodb.ScanOutput, error) {
	var items []map[string]types.AttributeValue
	for _, item := range m.Items {
		matches, err := conditionHolds(item, aws.ToString(params.FilterExpression), params.ExpressionAttributeNames, params.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		if matches {
			items = append(items, item)
		}
	}

	sortByCreation(items, false)
	return &dynamodb.ScanOutput{Items: items}, nil
}

// keyCondition reads the single equality a query is keyed on, and reports the attribute
// it names together with the value it compares against.
func keyCondition(
	expression string,
	names map[string]string,
	values map[string]types.AttributeValue,
) (string, types.AttributeValue, error) {
	alias, placeholder, ok := strings.Cut(expression, " = ")
	if !ok {
		return "", nil, fmt.Errorf("key condition %q is not an equality, which this table cannot evaluate", expression)
	}
	name, err := resolveName(names, strings.TrimSpace(alias))
	if err != nil {
		return "", nil, err
	}
	value, err := resolveValue(values, strings.TrimSpace(placeholder))
	if err != nil {
		return "", nil, err
	}
	return name, value, nil
}

// conditionHolds evaluates a condition against one item.
//
// Only the two shapes the stores write are understood: a conjunction of equalities, which
// is how a state transition guards itself, and a disjunction admitting an item that
// carries no mechanism at all, which is how the older mechanism claims the records
// written before there were two. An empty expression admits everything, since a read or a
// write without one is unconditional.
func conditionHolds(
	item map[string]types.AttributeValue,
	expression string,
	names map[string]string,
	values map[string]types.AttributeValue,
) (bool, error) {
	if strings.TrimSpace(expression) == "" {
		return true, nil
	}

	for alternative := range strings.SplitSeq(expression, " OR ") {
		holds, err := conjunctionHolds(item, alternative, names, values)
		if err != nil {
			return false, err
		}
		if holds {
			return true, nil
		}
	}
	return false, nil
}

// conjunctionHolds evaluates one alternative: every clause in it has to hold.
func conjunctionHolds(
	item map[string]types.AttributeValue,
	expression string,
	names map[string]string,
	values map[string]types.AttributeValue,
) (bool, error) {
	for clause := range strings.SplitSeq(expression, " AND ") {
		holds, err := clauseHolds(item, strings.TrimSpace(clause), names, values)
		if err != nil {
			return false, err
		}
		if !holds {
			return false, nil
		}
	}
	return true, nil
}

// clauseHolds evaluates a single clause: an equality, or a test that an attribute is
// absent.
func clauseHolds(
	item map[string]types.AttributeValue,
	clause string,
	names map[string]string,
	values map[string]types.AttributeValue,
) (bool, error) {
	if alias, found := strings.CutPrefix(clause, "attribute_not_exists("); found {
		name, err := resolveName(names, strings.TrimSuffix(alias, ")"))
		if err != nil {
			return false, err
		}
		_, present := item[name]
		return !present, nil
	}

	alias, placeholder, ok := strings.Cut(clause, " = ")
	if !ok {
		return false, fmt.Errorf("clause %q is neither an equality nor an absence test, which is all this table can evaluate", clause)
	}
	name, err := resolveName(names, strings.TrimSpace(alias))
	if err != nil {
		return false, err
	}
	want, err := resolveValue(values, strings.TrimSpace(placeholder))
	if err != nil {
		return false, err
	}
	return equalAttributes(item[name], want), nil
}

// applyUpdate carries out the assignments and removals of an update expression of the
// form "SET #a = :a, #b = :b REMOVE #c".
func applyUpdate(item map[string]types.AttributeValue, params *dynamodb.UpdateItemInput) error {
	expression := strings.TrimSpace(aws.ToString(params.UpdateExpression))
	if expression == "" {
		return fmt.Errorf("an update with no expression changes nothing")
	}

	assignments := expression
	var removals string
	if before, after, found := strings.Cut(expression, "REMOVE "); found {
		assignments, removals = strings.TrimSpace(before), after
	}

	if assignments != "" {
		clauses, found := strings.CutPrefix(assignments, "SET ")
		if !found {
			return fmt.Errorf("update expression %q is not one this table can apply", expression)
		}
		for _, assignment := range splitClauses(clauses) {
			alias, placeholder, ok := strings.Cut(assignment, " = ")
			if !ok {
				return fmt.Errorf("assignment %q is not of the form '#name = :value'", assignment)
			}
			name, err := resolveName(params.ExpressionAttributeNames, strings.TrimSpace(alias))
			if err != nil {
				return err
			}
			value, err := resolveValue(params.ExpressionAttributeValues, strings.TrimSpace(placeholder))
			if err != nil {
				return err
			}
			item[name] = value
		}
	}

	for _, alias := range splitClauses(removals) {
		name, err := resolveName(params.ExpressionAttributeNames, alias)
		if err != nil {
			return err
		}
		delete(item, name)
	}
	return nil
}

func splitClauses(list string) []string {
	var clauses []string
	for clause := range strings.SplitSeq(list, ",") {
		if trimmed := strings.TrimSpace(clause); trimmed != "" {
			clauses = append(clauses, trimmed)
		}
	}
	return clauses
}

// resolveName resolves the attribute an expression token refers to.
//
// A token may be an alias or the attribute's own name: aliasing is only required where the
// name is a word the table reserves, and the stores here alias in one expression and not in
// another. Both forms have to be understood, or a read this table can answer perfectly well
// fails for the way its expression happened to be written.
func resolveName(names map[string]string, token string) (string, error) {
	if !strings.HasPrefix(token, "#") {
		return token, nil
	}
	name, ok := names[token]
	if !ok {
		return "", fmt.Errorf("expression refers to %s, which is not aliased in %v", token, names)
	}
	return name, nil
}

func resolveValue(values map[string]types.AttributeValue, placeholder string) (types.AttributeValue, error) {
	value, ok := values[placeholder]
	if !ok {
		return nil, fmt.Errorf("expression refers to %s, for which no value was supplied", placeholder)
	}
	return value, nil
}

// equalAttributes compares two attribute values. Only the string form is compared, since
// every key and every mechanism marker in this table is one; anything else is unequal
// rather than silently matching.
func equalAttributes(got, want types.AttributeValue) bool {
	gotString, ok := got.(*types.AttributeValueMemberS)
	if !ok {
		return false
	}
	wantString, ok := want.(*types.AttributeValueMemberS)
	if !ok {
		return false
	}
	return gotString.Value == wantString.Value
}

// sortByCreation orders items by their creation time, tie-broken on the partition key so
// two records written in the same second keep a stable order between runs. The timestamps
// are RFC 3339 in UTC, which sorts correctly as text.
func sortByCreation(items []map[string]types.AttributeValue, ascending bool) {
	slices.SortFunc(items, func(a, b map[string]types.AttributeValue) int {
		order := strings.Compare(stringAttribute(a, createdAtAttribute), stringAttribute(b, createdAtAttribute))
		if order == 0 {
			order = strings.Compare(stringAttribute(a, "shareId"), stringAttribute(b, "shareId"))
		}
		if ascending {
			return order
		}
		return -order
	})
}

func stringAttribute(item map[string]types.AttributeValue, name string) string {
	if attr, ok := item[name].(*types.AttributeValueMemberS); ok {
		return attr.Value
	}
	return ""
}
