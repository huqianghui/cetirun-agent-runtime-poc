# Gmail and Google Calendar

Both services use the selected Google account and its actual OAuth scopes.
Google records that account as the executor; CetiRun also identifies the Agent.
Choose only a service returned for that connection by `services_list`.

## Gmail

Service: `google_gmail`. Origin: `https://gmail.googleapis.com`.
Base path: `/gmail/v1/`.

- GET `/users/me/messages`, query `{"maxResults":"20","q":"is:unread"}`.
- GET `/users/me/messages/{id}`, query `{"format":"full"}`.
- POST `/users/me/messages/send`, body
  `{"raw":"<base64url-encoded RFC 5322 message>"}`.

A message list returns IDs, not complete messages. Fetch the relevant messages
separately. Continue with `nextPageToken` as query `pageToken` on another call.
For sending, build the intended recipients, subject and body into the RFC 5322
message before base64url encoding. Sending requires Gmail send scope and the
user's authorization for the message. Other operations depend on the actual
granted scopes; enabling Gmail does not mean every Gmail API is permitted.

## Google Calendar

Service: `google_calendar`. Origin: `https://www.googleapis.com`.
Base path: `/calendar/v3/`.

- GET `/users/me/calendarList`, query `{"maxResults":"20"}`.
- GET `/calendars/primary/events`, query includes string `timeMin`, `timeMax`,
  `singleEvents` and `maxResults`. Times are RFC3339 with an offset.
- POST `/calendars/primary/events`, body such as
  `{"summary":"Example","start":{"dateTime":"2030-01-10T10:00:00Z"},"end":{"dateTime":"2030-01-10T10:30:00Z"}}`.

Use the intended calendar ID and URL-encode path segments. Continue with
`nextPageToken` as query `pageToken` only as needed. Adding attendees or
requesting updates may send invitations; ensure those effects are within the
user's request. Inspect provider state before retrying an uncertain event
creation or mail send, since repeated calls may duplicate their effects.
