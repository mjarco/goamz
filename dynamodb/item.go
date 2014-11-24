package dynamodb

import simplejson "github.com/bitly/go-simplejson"
import (
	"errors"
	"fmt"
	"log"
	"time"
)

const maxNumberOfRetry = 4

type BatchGetItem struct {
	Server *Server
	Keys   map[*Table][]Key
}

type BatchWriteItem struct {
	Server      *Server
	ItemActions map[*Table]map[string][][]Attribute
}

func (t *Table) BatchGetItems(keys []Key) *BatchGetItem {
	batchGetItem := &BatchGetItem{t.Server, make(map[*Table][]Key)}

	batchGetItem.Keys[t] = keys
	return batchGetItem
}

func (t *Table) BatchWriteItems(itemActions map[string][][]Attribute) *BatchWriteItem {
	batchWriteItem := &BatchWriteItem{t.Server, make(map[*Table]map[string][][]Attribute)}

	batchWriteItem.ItemActions[t] = itemActions
	return batchWriteItem
}

func (batchGetItem *BatchGetItem) AddTable(t *Table, keys *[]Key) *BatchGetItem {
	batchGetItem.Keys[t] = *keys
	return batchGetItem
}

func (batchWriteItem *BatchWriteItem) AddTable(t *Table, itemActions *map[string][][]Attribute) *BatchWriteItem {
	batchWriteItem.ItemActions[t] = *itemActions
	return batchWriteItem
}

func (batchGetItem *BatchGetItem) Execute() (map[string][]map[string]*Attribute, error) {
	q := NewEmptyQuery()
	q.AddGetRequestItems(batchGetItem.Keys)

	jsonResponse, err := batchGetItem.Server.queryServer(target("BatchGetItem"), q)
	if err != nil {
		return nil, err
	}

	json, err := simplejson.NewJson(jsonResponse)

	if err != nil {
		return nil, err
	}

	results := make(map[string][]map[string]*Attribute)

	tables, err := json.Get("Responses").Map()
	if err != nil {
		message := fmt.Sprintf("Unexpected response %s", jsonResponse)
		return nil, errors.New(message)
	}

	for table, entries := range tables {
		var tableResult []map[string]*Attribute

		jsonEntriesArray, ok := entries.([]interface{})
		if !ok {
			message := fmt.Sprintf("Unexpected response %s", jsonResponse)
			return nil, errors.New(message)
		}

		for _, entry := range jsonEntriesArray {
			item, ok := entry.(map[string]interface{})
			if !ok {
				message := fmt.Sprintf("Unexpected response %s", jsonResponse)
				return nil, errors.New(message)
			}

			unmarshalledItem := parseAttributes(item)
			tableResult = append(tableResult, unmarshalledItem)
		}

		results[table] = tableResult
	}

	return results, nil
}

func (batchWriteItem *BatchWriteItem) Execute() (map[string]interface{}, error) {
	q := NewEmptyQuery()
	q.AddWriteRequestItems(batchWriteItem.ItemActions)

	jsonResponse, err := batchWriteItem.Server.queryServer(target("BatchWriteItem"), q)

	if err != nil {
		return nil, err
	}

	json, err := simplejson.NewJson(jsonResponse)

	if err != nil {
		return nil, err
	}

	unprocessed, err := json.Get("UnprocessedItems").Map()
	if err != nil {
		message := fmt.Sprintf("Unexpected response %s", jsonResponse)
		return nil, errors.New(message)
	}

	if len(unprocessed) == 0 {
		return nil, nil
	} else {
		return unprocessed, errors.New("One or more unprocessed items.")
	}

}

func (t *Table) GetItem(key *Key) (map[string]*Attribute, error) {
	return t.getItem(key, false)
}

func (t *Table) GetItemConsistent(key *Key, consistentRead bool) (map[string]*Attribute, error) {
	return t.getItem(key, consistentRead)
}

func (t *Table) getItem(key *Key, consistentRead bool) (map[string]*Attribute, error) {
	q := NewQuery(t)
	q.AddKey(t, key)

	if consistentRead {
		q.ConsistentRead(consistentRead)
	}

	jsonResponse, err := t.Server.queryServer(target("GetItem"), q)
	if err != nil {
		return nil, err
	}

	json, err := simplejson.NewJson(jsonResponse)
	if err != nil {
		return nil, err
	}

	itemJson, ok := json.CheckGet("Item")
	if !ok {
		// We got an empty from amz. The item doesn't exist.
		return nil, ErrNotFound
	}

	item, err := itemJson.Map()
	if err != nil {
		message := fmt.Sprintf("Unexpected response %s", jsonResponse)
		return nil, errors.New(message)
	}

	return parseAttributes(item), nil

}

func (t *Table) PutItem(hashKey string, rangeKey string, attributes []Attribute) (bool, error) {
	return t.putItem(hashKey, rangeKey, attributes, nil, nil)
}

func (t *Table) ConditionalPutItem(hashKey, rangeKey string, attributes, expected []Attribute) (bool, error) {
	return t.putItem(hashKey, rangeKey, attributes, expected, nil)
}

func (t *Table) ConditionExpressionPutItem(hashKey, rangeKey string, attributes []Attribute, condition *Expression) (bool, error) {
	return t.putItem(hashKey, rangeKey, attributes, nil, condition)
}

func (t *Table) putItem(hashKey, rangeKey string, attributes, expected []Attribute, condition *Expression) (bool, error) {
	if len(attributes) == 0 {
		return false, errors.New("At least one attribute is required.")
	}

	q := NewQuery(t)

	keys := t.Key.Clone(hashKey, rangeKey)
	attributes = append(attributes, keys...)

	q.AddItem(attributes)

	if expected != nil {
		q.AddExpected(expected)
	}

	if condition != nil {
		q.AddConditionExpression(condition)
	}

	var jsonResponse []byte
	var err error
	// based on:
	// http://docs.aws.amazon.com/amazondynamodb/latest/developerguide/ErrorHandling.html#APIRetries
	currentRetry := uint(0)
	for {
		jsonResponse, err = t.Server.queryServer(target("PutItem"), q)
		if currentRetry >= maxNumberOfRetry {
			break
		}

		retry := false
		if err != nil {
			log.Printf("Error requesting from Amazon, request was: %#v\n response is:%#v\n and error is: %#v\n", q, string(jsonResponse), err)
			if err, ok := err.(*Error); ok {
				retry = (err.StatusCode == 500) ||
					(err.Code == "ThrottlingException") ||
					(err.Code == "ProvisionedThroughputExceededException")
			}
		}

		if !retry {
			break
		}

		log.Printf("Retrying in %v ms\n", (1<<currentRetry)*50)
		<-time.After((1 << currentRetry) * 50 * time.Millisecond)
		currentRetry += 1
	}

	if err != nil {
		return false, err
	}

	_, err = simplejson.NewJson(jsonResponse)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (t *Table) deleteItem(key *Key, expected []Attribute, condition *Expression) (bool, error) {
	q := NewQuery(t)
	q.AddKey(t, key)

	if expected != nil {
		q.AddExpected(expected)
	}

	if condition != nil {
		q.AddConditionExpression(condition)
	}

	jsonResponse, err := t.Server.queryServer(target("DeleteItem"), q)

	if err != nil {
		return false, err
	}

	_, err = simplejson.NewJson(jsonResponse)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (t *Table) DeleteItem(key *Key) (bool, error) {
	return t.deleteItem(key, nil, nil)
}

func (t *Table) ConditionalDeleteItem(key *Key, expected []Attribute) (bool, error) {
	return t.deleteItem(key, expected, nil)
}

func (t *Table) ConditionExpressionDeleteItem(key *Key, condition *Expression) (bool, error) {
	return t.deleteItem(key, nil, condition)
}

func (t *Table) AddAttributes(key *Key, attributes []Attribute) (bool, error) {
	return t.modifyAttributes(key, attributes, nil, nil, "ADD")
}

func (t *Table) UpdateAttributes(key *Key, attributes []Attribute) (bool, error) {
	return t.modifyAttributes(key, attributes, nil, nil, "PUT")
}

func (t *Table) DeleteAttributes(key *Key, attributes []Attribute) (bool, error) {
	return t.modifyAttributes(key, attributes, nil, nil, "DELETE")
}

func (t *Table) ConditionalAddAttributes(key *Key, attributes, expected []Attribute) (bool, error) {
	return t.modifyAttributes(key, attributes, expected, nil, "ADD")
}

func (t *Table) ConditionalUpdateAttributes(key *Key, attributes, expected []Attribute) (bool, error) {
	return t.modifyAttributes(key, attributes, expected, nil, "PUT")
}

func (t *Table) ConditionalDeleteAttributes(key *Key, attributes, expected []Attribute) (bool, error) {
	return t.modifyAttributes(key, attributes, expected, nil, "DELETE")
}

func (t *Table) ConditionExpressionAddAttributes(key *Key, attributes []Attribute, condition *Expression) (bool, error) {
	return t.modifyAttributes(key, attributes, nil, condition, "ADD")
}

func (t *Table) ConditionExpressionUpdateAttributes(key *Key, attributes []Attribute, condition *Expression) (bool, error) {
	return t.modifyAttributes(key, attributes, nil, condition, "PUT")
}

func (t *Table) ConditionExpressionDeleteAttributes(key *Key, attributes []Attribute, condition *Expression) (bool, error) {
	return t.modifyAttributes(key, attributes, nil, condition, "DELETE")
}

func (t *Table) modifyAttributes(key *Key, attributes, expected []Attribute, condition *Expression, action string) (bool, error) {

	if len(attributes) == 0 {
		return false, errors.New("At least one attribute is required.")
	}

	q := NewQuery(t)
	q.AddKey(t, key)
	q.AddUpdates(attributes, action)

	if expected != nil {
		q.AddExpected(expected)
	}

	if condition != nil {
		q.AddConditionExpression(condition)
	}

	jsonResponse, err := t.Server.queryServer(target("UpdateItem"), q)

	if err != nil {
		return false, err
	}

	_, err = simplejson.NewJson(jsonResponse)
	if err != nil {
		return false, err
	}

	return true, nil
}

func parseAttributes(s map[string]interface{}) map[string]*Attribute {
	results := map[string]*Attribute{}

	for key, value := range s {
		if v, ok := value.(map[string]interface{}); ok {
			if val, ok := v[TYPE_STRING].(string); ok {
				results[key] = &Attribute{
					Type:  TYPE_STRING,
					Name:  key,
					Value: val,
				}
			} else if val, ok := v[TYPE_NUMBER].(string); ok {
				results[key] = &Attribute{
					Type:  TYPE_NUMBER,
					Name:  key,
					Value: val,
				}
			} else if val, ok := v[TYPE_BINARY].(string); ok {
				results[key] = &Attribute{
					Type:  TYPE_BINARY,
					Name:  key,
					Value: val,
				}
			} else if vals, ok := v[TYPE_STRING_SET].([]interface{}); ok {
				arry := make([]string, len(vals))
				for i, ivalue := range vals {
					if val, ok := ivalue.(string); ok {
						arry[i] = val
					}
				}
				results[key] = &Attribute{
					Type:      TYPE_STRING_SET,
					Name:      key,
					SetValues: arry,
				}
			} else if vals, ok := v[TYPE_NUMBER_SET].([]interface{}); ok {
				arry := make([]string, len(vals))
				for i, ivalue := range vals {
					if val, ok := ivalue.(string); ok {
						arry[i] = val
					}
				}
				results[key] = &Attribute{
					Type:      TYPE_NUMBER_SET,
					Name:      key,
					SetValues: arry,
				}
			} else if vals, ok := v[TYPE_BINARY_SET].([]interface{}); ok {
				arry := make([]string, len(vals))
				for i, ivalue := range vals {
					if val, ok := ivalue.(string); ok {
						arry[i] = val
					}
				}
				results[key] = &Attribute{
					Type:      TYPE_BINARY_SET,
					Name:      key,
					SetValues: arry,
				}
			}
		} else {
			log.Printf("type assertion to map[string] interface{} failed for : %s\n ", value)
		}

	}

	return results
}
