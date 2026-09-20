@live
Feature: Live sandbox gateway smoke tests

  Scenario Outline: GitHub issue creation follows the selected mode
    Given a unique GitHub issue for mode "<mode>"
    When the sandbox creates the GitHub issue
    Then issue creation is "<result>"

    Examples:
      | mode    | result  |
      | browse  | denied  |
      | local   | denied  |
      | publish | allowed |

  Scenario Outline: Public HTTPS is available in every mode
    Given a unique HTTPS request for mode "<mode>"
    When the sandbox GETs httpbin
    Then the HTTPS response is successful

    Examples:
      | mode    |
      | browse  |
      | local   |
      | publish |
