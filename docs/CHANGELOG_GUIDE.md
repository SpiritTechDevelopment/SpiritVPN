# Руководство по CHANGELOG

## Автоматическое обновление

CHANGELOG.md автоматически обновляется при мерже PR в `main` с помощью GitHub Actions.

## Как это работает

1. При создании PR добавьте **labels** для категоризации изменений:
   - `feature` / `enhancement` → **Added**
   - `bugfix` / `fix` → **Fixed**
   - `documentation` → **Documentation**
   - `refactor` → **Changed**
   - `breaking` → **Breaking Changes**

2. При мерже PR в `main`:
   - GitHub Action автоматически добавляет запись в секцию `[Unreleased]`
   - Запись включает: название PR, номер, автора
   - Запись помещается в соответствующую категорию по label

## Структура CHANGELOG

```markdown
## [Unreleased]

### Added
- Новая функциональность

### Changed
- Изменения в существующей функциональности

### Fixed
- Исправления багов

### Removed
- Удаленная функциональность

### Breaking Changes
- Несовместимые изменения
```

## Создание релиза

Когда готовы создать новый релиз:

1. Вручную переместите записи из `[Unreleased]` в новую версию:

```markdown
## [Unreleased]

## [1.1.0] - 2026-01-15

### Added
- Structured logger package ([#17](https://github.com/RomanRyabinkin/SpiritVPN/pull/17)) by @RomanRyabinkin

### Fixed
- Logger errcheck errors ([#18](https://github.com/RomanRyabinkin/SpiritVPN/pull/18)) by @RomanRyabinkin
```

2. Обновите ссылки в конце файла:

```markdown
[Unreleased]: https://github.com/RomanRyabinkin/SpiritVPN/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/RomanRyabinkin/SpiritVPN/compare/v1.0.0...v1.1.0
```

3. Создайте Git tag:

```bash
git tag -a v1.1.0 -m "Release v1.1.0"
git push origin v1.1.0
```

4. Создайте GitHub Release, скопировав содержимое секции из CHANGELOG

## Семантическое версионирование

Следуем [SemVer](https://semver.org/):

- **MAJOR** (1.x.x) - Breaking changes
- **MINOR** (x.1.x) - New features (backward compatible)
- **PATCH** (x.x.1) - Bug fixes (backward compatible)

## Примеры

### PR с новой функциональностью
```
Title: Add structured logger package
Labels: feature, enhancement
```

Запись в CHANGELOG:
```markdown
### Added
- Add structured logger package ([#17](https://github.com/RomanRyabinkin/SpiritVPN/pull/17)) by @RomanRyabinkin
```

### PR с исправлением
```
Title: Fix logger errcheck errors
Labels: bugfix
```

Запись в CHANGELOG:
```markdown
### Fixed
- Fix logger errcheck errors ([#18](https://github.com/RomanRyabinkin/SpiritVPN/pull/18)) by @RomanRyabinkin
```

### PR с документацией
```
Title: Update logger documentation
Labels: documentation
```

Запись в CHANGELOG:
```markdown
### Documentation
- Update logger documentation ([#19](https://github.com/RomanRyabinkin/SpiritVPN/pull/19)) by @RomanRyabinkin
```

## Best Practices

1. **Используйте понятные названия PR** - они попадут в CHANGELOG
2. **Добавляйте labels** - для правильной категоризации
3. **Один PR - одна задача** - для чистого CHANGELOG
4. **Описывайте изменения в PR** - упрощает review и CHANGELOG
5. **Регулярно создавайте релизы** - не копите много изменений

## Ручное редактирование

Если нужно вручную отредактировать CHANGELOG:

1. Добавьте запись в нужную категорию под `[Unreleased]`
2. Используйте формат: `- Описание ([#123](link)) by @author`
3. Коммитьте напрямую в `main` или через PR

## Уведомления

При обновлении CHANGELOG автоматически отправляется уведомление в топик **Review** (Thread ID: 20) с информацией о мерже PR.
