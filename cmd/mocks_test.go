package cmd

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// mockDDB is an in-memory DynamoDB mock backing a real auth.ShareStore. It stores items
// keyed by shareId and applies revokedAt/expiresAt updates so command-driven state
// transitions (revoke, extend) are observable across calls.
type mockDDB struct {
	items map[string]map[string]types.AttributeValue
}

var _ internalaws.DynamoDBClient = (*mockDDB)(nil)

func newMockDDB() *mockDDB {
	return &mockDDB{items: make(map[string]map[string]types.AttributeValue)}
}

func (m *mockDDB) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if idAttr, ok := params.Item["shareId"].(*types.AttributeValueMemberS); ok {
		m.items[idAttr.Value] = params.Item
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (m *mockDDB) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if idAttr, ok := params.Key["shareId"].(*types.AttributeValueMemberS); ok {
		if item, exists := m.items[idAttr.Value]; exists {
			return &dynamodb.GetItemOutput{Item: item}, nil
		}
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (m *mockDDB) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if idAttr, ok := params.Key["shareId"].(*types.AttributeValueMemberS); ok {
		if item, exists := m.items[idAttr.Value]; exists {
			if v, ok := params.ExpressionAttributeValues[":revokedAt"]; ok {
				item["revokedAt"] = v
			}
			if v, ok := params.ExpressionAttributeValues[":expiresAt"]; ok {
				item["expiresAt"] = v
			}
		}
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (m *mockDDB) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	var items []map[string]types.AttributeValue
	accountID := ""
	if v, ok := params.ExpressionAttributeValues[":accountId"].(*types.AttributeValueMemberS); ok {
		accountID = v.Value
	}
	for _, item := range m.items {
		if acct, ok := item["researcherAccountId"].(*types.AttributeValueMemberS); ok && acct.Value == accountID {
			items = append(items, item)
		}
	}
	return &dynamodb.QueryOutput{Items: items}, nil
}

func (m *mockDDB) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	var items []map[string]types.AttributeValue
	for _, item := range m.items {
		items = append(items, item)
	}
	return &dynamodb.ScanOutput{Items: items}, nil
}

func (m *mockDDB) DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}
