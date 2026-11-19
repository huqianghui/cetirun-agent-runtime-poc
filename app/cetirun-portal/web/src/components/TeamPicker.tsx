// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Field, Select } from "@fluentui/react-components";
import { useTeams } from "../hooks/useTeams";

interface TeamPickerProps {
  value: string;
  onChange: (value: string) => void;
  /** A required Team offers no "(none)" choice. */
  required?: boolean;
}

export function TeamPicker({ value, onChange, required = false }: TeamPickerProps) {
  const { data: teams = [], isLoading } = useTeams();
  const noTeams = required && !isLoading && teams.length === 0;

  return (
    <Field
      label={required ? "Team" : "Team (optional)"}
      required={required}
      validationState={noTeams ? "warning" : undefined}
      validationMessage={noTeams ? "No Teams exist yet. Ask an administrator to create one." : undefined}
    >
      <Select
        value={value}
        onChange={(_, d) => onChange(d.value)}
        disabled={isLoading}
      >
        {required ? (
          <option value="" disabled>
            Choose a Team
          </option>
        ) : (
          <option value="">(none)</option>
        )}
        {teams.map((t) => (
          <option key={t.name} value={t.name}>
            {t.name}
          </option>
        ))}
      </Select>
    </Field>
  );
}
