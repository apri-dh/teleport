/**
 * Teleport
 * Copyright (C) 2026  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

import { LEGACY_THEME_COLORS } from '@gravitational/design-system';

import { sharedColors } from './sharedStyles';
import type { Theme, ThemeDefinition } from './types';

/**
 * Combines a `ThemeDefinition` with the legacy color palette to produce a
 * complete `Theme`. Use anywhere a theme is fed into a styled-components
 * `ThemeProvider` outside the runtime providers (tests, storybook, error
 * fallbacks rendered above the runtime provider).
 */
export function resolveTheme(definition: ThemeDefinition): Theme {
  return {
    ...definition,
    colors: {
      ...sharedColors,
      ...LEGACY_THEME_COLORS,
    },
  };
}
