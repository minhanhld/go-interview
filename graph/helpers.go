package graph

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"encoding/json"

	"github.com/example/ds-technical-assessment/graph/model"
)

func encodeCursor(uri string) string {
	return base64.StdEncoding.EncodeToString([]byte(uri))
}


func decodeCursor(cursor string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		// %w is a special verb in fmt.Errorf that WRAPS the original error.
		// This preserves the original error so callers can inspect it with
		// errors.Is() or errors.As() if needed.
		return "", fmt.Errorf("invalid cursor: %w", err)
	}
	return string(b), nil
}

func getRows(ctx context.Context, db *sql.DB, elements []*model.Element) (*sql.Rows, error) {
	elementsURIs := make([]any, len(elements))
	for i, e := range elements {
		elementsURIs[i] = e.URI
	}
	placeholders := make([]string, len(elementsURIs))
	for i := range placeholders {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	placeholdersList := strings.Join(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT
			efv.uri,
			efv.element_uri,
			efv.value_text,
			efv.value_number,
			efv.value_date,
			efv.value_boolean,
			efv.value_json,
			f.uri  AS field_uri,
			f.name AS field_name,
			f.field_type,
			f.options,
			f.required
		FROM element_field_values efv
		JOIN fields f ON efv.field_uri = f.uri
		WHERE efv.element_uri IN (%s)
		ORDER BY efv.element_uri, f.name
	`, placeholdersList)

	return db.QueryContext(ctx, query, elementsURIs...)	
}

// loadFieldValues fetches all field values for a batch of element in ONE query then distributes them accordingly
//
// This avoids the "N+1" query issue
func loadFieldValues(ctx context.Context, db *sql.DB, elements []*model.Element) error {
	elementsByURI := make(map[string]*model.Element, len(elements))
	for _, elemCopy := range elements {
		elementsByURI[elemCopy.URI] = elemCopy
	}

	rows, err := getRows(ctx, db, elements)
	if err != nil {
		return fmt.Errorf("querying field values: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		var (
			fieldValueURI	string
			elemURI    		string
			valueText  		sql.NullString
			valueNum   		sql.NullFloat64
			valueDate  		sql.NullInt64
			valueBool  		sql.NullBool
			valueJSON  		sql.NullString
			fieldURI   		string
			fieldName  		string
			fieldDataType  	string
			fieldOptions	sql.NullString
			fieldRequired	bool
		)

		err := rows.Scan(
			&fieldValueURI,
			&elemURI,
			&valueText,
			&valueNum,
			&valueDate,
			&valueBool,
			&valueJSON,
			&fieldURI,
			&fieldName,
			&fieldDataType,
			&fieldOptions,
			&fieldRequired,
		)
		if err != nil {
			return fmt.Errorf("scanning field value: %w", err)
		}
		var value any
		switch {
		case valueText.Valid:
			value = valueText.String
		case valueNum.Valid:
			value = valueNum.Float64
		case valueBool.Valid:
			value = valueBool.Bool
		case valueDate.Valid:
			value = time.UnixMilli(valueDate.Int64).UTC().Format(time.RFC3339)
		case valueJSON.Valid:
			value = valueJSON.String
		}

		var options map[string]any
		if fieldOptions.Valid {
			err := json.Unmarshal([]byte(fieldOptions.String), &options)
			if (err != nil) {
				return fmt.Errorf("scanning field value: %w", err)
			}
		}

		fieldValue := &model.FieldValue{
			URI:   fieldValueURI,
			Value: value,
			Field: &model.Field{
				URI:      	fieldURI,
				Name:     	fieldName,
				DataType:	fieldDataType,
				Options:	options["options"],
				Required:	fieldRequired,
			},
		}
		if elem, ok := elementsByURI[elemURI]; ok {
			elem.FieldValues = append(elem.FieldValues, fieldValue)
		}
	}
	return rows.Err()
}