package main

// openAPI describes the REST API in rest.go. Keep the two in step:
// TestOpenAPIListsEveryRoute compares them.
func (a *App) openAPI() map[string]any {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	strList := map[string]any{"type": "array", "items": str}
	addresses := map[string]any{
		"oneOf":       []any{strList, str},
		"description": "List of addresses, or one string separated by commas.",
	}
	attachments := map[string]any{
		"type":        "array",
		"description": "Files named by reference. With multipart, every file part is attached as well.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"upload_id":      map[string]any{"type": "string", "description": "From POST /uploads."},
				"filename":       str,
				"url":            map[string]any{"type": "string", "description": "Public https URL to download from."},
				"content_base64": str,
				"mime_type":      str,
			},
		},
	}
	idParam := map[string]any{"name": "id", "in": "path", "required": true, "schema": integer}
	query := func(name, desc string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": "query", "description": desc, "schema": schema}
	}
	fields := query("fields", "Comma-separated fields to return besides id.", str)
	jsonResult := map[string]any{"description": "OK", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	errors := map[string]any{
		"400": map[string]any{"description": "Invalid arguments."},
		"401": map[string]any{"description": "Missing, invalid or expired token."},
		"403": map[string]any{"description": "Not allowed for this FreeScout user."},
		"404": map[string]any{"description": "Not found."},
		"413": map[string]any{"description": "More than 20 MB of attachments."},
		"422": map[string]any{"description": "Refused by FreeScout."},
	}
	responses := func(status string) map[string]any {
		out := map[string]any{status: jsonResult}
		for k, v := range errors {
			out[k] = v
		}
		return out
	}
	body := func(props map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		multipart := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true,
			"description": "The same fields as form fields (or all of them as JSON in a field named payload); every file part becomes an attachment."}
		for k, v := range props {
			multipart["properties"].(map[string]any)[k] = v
		}
		multipart["properties"].(map[string]any)["file"] = map[string]any{"type": "string", "format": "binary"}
		return map[string]any{"required": true, "content": map[string]any{
			"application/json":    map[string]any{"schema": schema},
			"multipart/form-data": map[string]any{"schema": multipart},
		}}
	}
	operation := func(summary string, params []any, requestBody map[string]any, status string) map[string]any {
		op := map[string]any{"summary": summary, "responses": responses(status)}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if requestBody != nil {
			op["requestBody"] = requestBody
		}
		return op
	}
	status := map[string]any{"type": "string", "enum": []string{"active", "pending", "closed", "spam"}}
	draftBody := map[string]any{"body": str, "cc": addresses, "bcc": addresses, "attachments": attachments}

	paths := map[string]any{
		"/me":        map[string]any{"get": operation("Who the token acts as.", nil, nil, "200")},
		"/mailboxes": map[string]any{"get": operation("Mailboxes you have access to.", nil, nil, "200")},
		"/conversations": map[string]any{"get": operation("Search conversations, newest reply first.", []any{
			query("query", "Free text: subject, number, customer, message text and addresses.", str),
			query("status", "", status),
			query("mailbox_id", "", integer),
			query("after", "YYYY-MM-DD", str),
			query("before", "YYYY-MM-DD", str),
			query("from", "Sender address or part of it.", str),
			query("to", "Recipient address or part of it.", str),
			query("state", "draft lists unsent new-email drafts.", map[string]any{"type": "string", "enum": []string{"published", "draft"}}),
			query("limit", "1-100, default 25.", integer),
			query("offset", "", integer),
			fields,
		}, nil, "200")},
		"/conversations/{id}": map[string]any{"get": operation("One conversation with its messages.", []any{
			idParam, fields, query("last_threads", "Only this many of the newest messages.", integer),
		}, nil, "200")},
		"/conversations/{id}/drafts": map[string]any{"post": operation("Save a reply draft. Never sends.", []any{idParam},
			body(draftBody, "body"), "201")},
		"/conversations/{id}/reply": map[string]any{"post": operation("Send a reply. This sends an email.", []any{idParam},
			body(map[string]any{"body": str, "cc": addresses, "bcc": addresses, "attachments": attachments, "status": status}, "body"), "201")},
		"/conversations/{id}/forward": map[string]any{"post": operation("Forward: a draft by default, sent right away with send=true.", []any{idParam},
			body(map[string]any{"to": addresses, "cc": addresses, "bcc": addresses, "body": str, "include_attachments": map[string]any{"type": "boolean"},
				"send": map[string]any{"type": "boolean"}, "attachments": attachments}, "to"), "201")},
		"/conversations/{id}/notes": map[string]any{"post": operation("Add an internal note.", []any{idParam},
			body(map[string]any{"body": str}, "body"), "201")},
		"/conversations/{id}/status": map[string]any{"post": operation("Change the status.", []any{idParam},
			body(map[string]any{"status": status}, "status"), "200")},
		"/conversations/{id}/assign": map[string]any{"post": operation("Assign, or unassign with an empty address.", []any{idParam},
			body(map[string]any{"assignee_email": str}), "200")},
		"/conversations/{id}/move": map[string]any{"post": operation("Move to another mailbox.", []any{idParam},
			body(map[string]any{"mailbox_id": integer}, "mailbox_id"), "200")},
		"/conversations/{id}/merge": map[string]any{"post": operation("Merge other conversations into this one.", []any{idParam},
			body(map[string]any{"merge_ids": map[string]any{"type": "array", "items": integer}}, "merge_ids"), "200")},
		"/drafts": map[string]any{"post": operation("Save a new email as a draft. Never sends.", nil,
			body(map[string]any{"mailbox_id": integer, "to": addresses, "cc": addresses, "bcc": addresses, "subject": str, "body": str, "attachments": attachments},
				"mailbox_id", "to", "subject", "body"), "201")},
		"/drafts/{id}": map[string]any{
			"patch": operation("Change a draft; only the given fields change, files are added.", []any{idParam},
				body(map[string]any{"to": addresses, "cc": addresses, "bcc": addresses, "subject": str, "body": str, "attachments": attachments,
					"remove_attachment_ids": map[string]any{"type": "array", "items": integer}}), "200"),
			"delete": operation("Discard a draft.", []any{idParam}, nil, "200"),
		},
		"/drafts/{id}/send": map[string]any{"post": operation("Send a draft. This sends an email.", []any{idParam},
			body(map[string]any{"status": status}), "200")},
		"/customers": map[string]any{"get": operation("Find customers by name, company or address.", []any{
			query("query", "At least two characters.", str),
		}, nil, "200")},
		"/attachments/{id}": map[string]any{"get": map[string]any{
			"summary":    "Download an attachment.",
			"parameters": []any{idParam},
			"responses": map[string]any{
				"200": map[string]any{"description": "The file.", "content": map[string]any{"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}},
				"401": errors["401"], "403": errors["403"], "404": errors["404"],
			},
		}},
		"/uploads": map[string]any{"post": map[string]any{
			"summary": "Upload files for later use as attachments (upload_id), kept for one hour.",
			"requestBody": map[string]any{"required": true, "content": map[string]any{"multipart/form-data": map[string]any{"schema": map[string]any{
				"type": "object", "properties": map[string]any{"file": map[string]any{"type": "string", "format": "binary"}},
			}}}},
			"responses": responses("201"),
		}},
		"/uploads/{id}": map[string]any{"delete": map[string]any{
			"summary":    "Discard an upload.",
			"parameters": []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": str}},
			"responses":  responses("200"),
		}},
	}

	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   a.cfg.AppName + " REST API",
			"version": version,
			"description": "The operations of the FreeScout MCP server for clients that can make HTTP requests. " +
				"Get a token with the MCP tool `token` (valid one hour, acts as the connected FreeScout user). " +
				"Only reply, forward with send=true and drafts/{id}/send send email.",
		},
		"servers":    []any{map[string]any{"url": a.cfg.BaseURL + apiBase}},
		"security":   []any{map[string]any{"bearer": []any{}}},
		"components": map[string]any{"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}}},
		"paths":      paths,
	}
}
